// droidfs-agent runs on the Android device (started through `adb exec:`) and
// serves file system requests from the host over stdin/stdout.
//
// It must never write anything but protocol frames to stdout/stderr: on old
// adbd both streams share the same socket.
package main

import (
	"bufio"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"droidfs/internal/proto"
)

const (
	workers     = 16
	idleExit    = 60 * time.Second // host pings every second or so
	fdIdleClose = 3 * time.Second
	fdCacheMax  = 64
)

type agent struct {
	wmu       sync.Mutex
	bw        *bufio.Writer
	fds       fdCache
	lastFrame atomic.Int64
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		os.Stdout.WriteString("droidfs-agent protocol 1\n")
		return
	}
	a := &agent{bw: bufio.NewWriterSize(os.Stdout, 256<<10)}
	a.fds.m = make(map[string]*fdEntry)
	a.lastFrame.Store(time.Now().UnixNano())
	if _, err := os.Stdout.WriteString(proto.Banner); err != nil {
		return
	}
	go a.housekeeping()

	in := bufio.NewReaderSize(os.Stdin, 256<<10)
	sem := make(chan struct{}, workers)
	for {
		id, op, payload, err := proto.ReadRequest(in)
		if err != nil {
			os.Exit(0) // host went away
		}
		a.lastFrame.Store(time.Now().UnixNano())
		switch op {
		case proto.OpPing:
			a.reply(id, proto.OK, nil)
			continue
		case proto.OpHello:
			st, resp := a.hello(payload)
			a.reply(id, st, resp)
			continue
		}
		// The read loop itself must never wait for a worker: if it did, a
		// burst of slow writes would also delay ping replies and the host
		// would take a merely slow device for a dead one.
		go func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			defer func() {
				if recover() != nil {
					a.reply(id, proto.ErrServerFault, []byte("agent panic"))
				}
			}()
			st, resp := a.handle(op, payload)
			a.reply(id, st, resp)
		}()
	}
}

func (a *agent) reply(id uint32, st proto.Status, payload []byte) {
	h := proto.ResponseHeader(id, st, len(payload))
	a.wmu.Lock()
	a.bw.Write(h[:])
	a.bw.Write(payload)
	err := a.bw.Flush()
	a.wmu.Unlock()
	if err != nil {
		os.Exit(0)
	}
}

// housekeeping closes idle descriptors and makes sure an orphaned agent
// (host gone without closing the stream) does not live forever.
func (a *agent) housekeeping() {
	for range time.Tick(time.Second) {
		if time.Since(time.Unix(0, a.lastFrame.Load())) > idleExit {
			os.Exit(0)
		}
		a.fds.sweep()
	}
}

func fail(err error) (proto.Status, []byte) {
	return statusOf(err), []byte(err.Error())
}

func statusOf(err error) proto.Status {
	var en syscall.Errno
	if errors.As(err, &en) {
		if en == syscall.ENOTSUP || en == syscall.EOPNOTSUPP || en == syscall.ENOSYS {
			return proto.ErrNotSupp
		}
		switch en {
		case syscall.EPERM:
			return proto.ErrPerm
		case syscall.ENOENT:
			return proto.ErrNoEnt
		case syscall.EACCES:
			return proto.ErrAcces
		case syscall.EEXIST:
			return proto.ErrExist
		case syscall.EXDEV:
			return proto.ErrXDev
		case syscall.ENOTDIR:
			return proto.ErrNotDir
		case syscall.EISDIR:
			return proto.ErrIsDir
		case syscall.EINVAL, syscall.ELOOP:
			return proto.ErrInval
		case syscall.EFBIG:
			return proto.ErrFBig
		case syscall.ENOSPC:
			return proto.ErrNoSpc
		case syscall.EROFS:
			return proto.ErrROFS
		case syscall.ENAMETOOLONG:
			return proto.ErrNameTooLong
		case syscall.ENOTEMPTY:
			return proto.ErrNotEmpty
		case syscall.EDQUOT:
			return proto.ErrDQuot
		case syscall.ESTALE:
			return proto.ErrStale
		}
		return proto.ErrIO
	}
	switch {
	case errors.Is(err, os.ErrNotExist):
		return proto.ErrNoEnt
	case errors.Is(err, os.ErrExist):
		return proto.ErrExist
	case errors.Is(err, os.ErrPermission):
		return proto.ErrAcces
	}
	return proto.ErrIO
}

func attrOf(fi os.FileInfo) proto.Attr {
	a := proto.Attr{
		Size:      uint64(fi.Size()),
		MtimeSec:  fi.ModTime().Unix(),
		MtimeNsec: uint32(fi.ModTime().Nanosecond()),
		Nlink:     1,
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		a.Mode = uint32(st.Mode)
		a.Nlink = uint32(st.Nlink)
		a.Used = uint64(st.Blocks) * 512
		a.Ino = uint64(st.Ino)
	} else if fi.IsDir() {
		a.Mode = proto.ModeDir | 0o755
	} else {
		a.Mode = proto.ModeReg | 0o644
	}
	return a
}

// statFollow resolves symlinks so that links like /sdcard behave as their
// targets; a dangling link is reported as the link itself.
func statFollow(path string) (os.FileInfo, error) {
	fi, err := os.Stat(path)
	if err == nil {
		return fi, nil
	}
	if li, lerr := os.Lstat(path); lerr == nil {
		return li, nil
	}
	return nil, err
}

func okAttr(fi os.FileInfo) (proto.Status, []byte) {
	var e proto.Enc
	e.Attr(attrOf(fi))
	return proto.OK, e.B
}

func (a *agent) hello(p []byte) (proto.Status, []byte) {
	d := proto.Dec{B: p}
	root := d.Str()
	if d.Err != nil {
		return proto.ErrInval, nil
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fail(err)
	}
	fi, err := os.Stat(resolved)
	if err != nil {
		return fail(err)
	}
	if !fi.IsDir() {
		return proto.ErrNotDir, []byte(root + ": not a directory")
	}
	var e proto.Enc
	e.U32(proto.Version)
	e.U32(uint32(os.Getuid()))
	e.Str(resolved)
	return proto.OK, e.B
}

func (a *agent) handle(op byte, p []byte) (proto.Status, []byte) {
	d := proto.Dec{B: p}
	path := d.Str()
	if d.Err != nil || path == "" {
		return proto.ErrInval, []byte("bad request")
	}
	switch op {
	case proto.OpStat:
		fi, err := statFollow(path)
		if err != nil {
			return fail(err)
		}
		return okAttr(fi)

	case proto.OpReadDir:
		return a.readDir(path)

	case proto.OpRead:
		off, count := d.U64(), d.U32()
		if d.Err != nil {
			return proto.ErrInval, nil
		}
		return a.read(path, off, count)

	case proto.OpWrite:
		off := d.U64()
		data := d.Rest()
		if d.Err != nil {
			return proto.ErrInval, nil
		}
		e, err := a.fds.acquire(path, true)
		if err != nil {
			return fail(err)
		}
		defer a.fds.release(e)
		// Attributes from just before and just after the write, taken
		// atomically with respect to other writes to this file, let the
		// NFS client tell its own changes from somebody else's.
		e.wmu.Lock()
		defer e.wmu.Unlock()
		pre, err := e.f.Stat()
		if err != nil {
			return fail(err)
		}
		if _, err := e.f.WriteAt(data, int64(off)); err != nil {
			return fail(err)
		}
		post, err := e.f.Stat()
		if err != nil {
			return fail(err)
		}
		var out proto.Enc
		out.Attr(attrOf(pre))
		out.Attr(attrOf(post))
		return proto.OK, out.B

	case proto.OpCreate:
		mode, excl, trunc := d.U32(), d.Bool(), d.Bool()
		if d.Err != nil {
			return proto.ErrInval, nil
		}
		flag := os.O_RDWR | os.O_CREATE
		if excl {
			flag |= os.O_EXCL
		}
		if trunc {
			flag |= os.O_TRUNC
		}
		a.fds.invalidate(path)
		f, err := os.OpenFile(path, flag, os.FileMode(mode&0o777))
		if err != nil {
			return fail(err)
		}
		fi, err := f.Stat()
		if err != nil {
			f.Close()
			return fail(err)
		}
		a.fds.put(path, f, true)
		return okAttr(fi)

	case proto.OpMkdir:
		mode := d.U32()
		if d.Err != nil {
			return proto.ErrInval, nil
		}
		if err := os.Mkdir(path, os.FileMode(mode&0o777)); err != nil {
			return fail(err)
		}
		fi, err := os.Lstat(path)
		if err != nil {
			return fail(err)
		}
		return okAttr(fi)

	case proto.OpRemove:
		a.fds.invalidate(path)
		if err := syscall.Unlink(path); err != nil {
			if err == syscall.EPERM { // Darwin's way of saying EISDIR
				if fi, e := os.Lstat(path); e == nil && fi.IsDir() {
					err = syscall.EISDIR
				}
			}
			return fail(err)
		}
		return proto.OK, nil

	case proto.OpRmdir:
		a.fds.invalidate(path)
		if err := syscall.Rmdir(path); err != nil {
			return fail(err)
		}
		return proto.OK, nil

	case proto.OpRename:
		to := d.Str()
		if d.Err != nil || to == "" {
			return proto.ErrInval, nil
		}
		a.fds.invalidate(path)
		a.fds.invalidate(to)
		if err := os.Rename(path, to); err != nil {
			return fail(err)
		}
		return proto.OK, nil

	case proto.OpTruncate:
		size := d.U64()
		if d.Err != nil {
			return proto.ErrInval, nil
		}
		if err := os.Truncate(path, int64(size)); err != nil {
			return fail(err)
		}
		return proto.OK, nil

	case proto.OpUtimes:
		sec, nsec := int64(d.U64()), d.U32()
		if d.Err != nil {
			return proto.ErrInval, nil
		}
		// A zero atime leaves the access time untouched.
		if err := os.Chtimes(path, time.Time{}, time.Unix(sec, int64(nsec))); err != nil {
			return fail(err)
		}
		return proto.OK, nil

	case proto.OpFsync:
		return a.fsync(path)

	case proto.OpStatfs:
		var st syscall.Statfs_t
		if err := syscall.Statfs(path, &st); err != nil {
			return fail(err)
		}
		var e proto.Enc
		e.U64(uint64(st.Bsize))
		e.U64(uint64(st.Blocks))
		e.U64(uint64(st.Bfree))
		e.U64(uint64(st.Bavail))
		e.U64(uint64(st.Files))
		e.U64(uint64(st.Ffree))
		return proto.OK, e.B

	case proto.OpSymlink:
		target := d.Str()
		if d.Err != nil {
			return proto.ErrInval, nil
		}
		if err := os.Symlink(target, path); err != nil {
			return fail(err)
		}
		fi, err := os.Lstat(path)
		if err != nil {
			return fail(err)
		}
		return okAttr(fi)

	case proto.OpReadlink:
		target, err := os.Readlink(path)
		if err != nil {
			return fail(err)
		}
		var e proto.Enc
		e.Str(target)
		return proto.OK, e.B
	}
	return proto.ErrNotSupp, []byte("unknown op")
}

func (a *agent) readDir(path string) (proto.Status, []byte) {
	f, err := os.Open(path)
	if err != nil {
		return fail(err)
	}
	names, err := f.Readdirnames(-1)
	f.Close()
	if err != nil && len(names) == 0 {
		return fail(err)
	}
	prefix := path
	if prefix != "/" {
		prefix += "/"
	}
	e := proto.Enc{B: make([]byte, 0, 4+len(names)*(proto.AttrLen+24))}
	e.U32(0)
	var n uint32
	for _, name := range names {
		fi, err := os.Lstat(prefix + name)
		if err != nil {
			continue // vanished while listing
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			if ti, err := os.Stat(prefix + name); err == nil {
				fi = ti
			}
		}
		e.Str(name)
		e.Attr(attrOf(fi))
		n++
	}
	e.B[0], e.B[1], e.B[2], e.B[3] = byte(n>>24), byte(n>>16), byte(n>>8), byte(n)
	return proto.OK, e.B
}

func (a *agent) read(path string, off uint64, count uint32) (proto.Status, []byte) {
	if count > proto.MaxIO {
		count = proto.MaxIO
	}
	e, err := a.fds.acquire(path, false)
	if err != nil {
		return fail(err)
	}
	defer a.fds.release(e)
	const head = 1 + proto.AttrLen
	buf := make([]byte, head+int(count))
	n, err := e.f.ReadAt(buf[head:], int64(off))
	eof := false
	if err == io.EOF {
		eof = true
	} else if err != nil {
		return fail(err)
	}
	fi, err := e.f.Stat()
	if err != nil {
		return fail(err)
	}
	h := proto.Enc{B: buf[:0]}
	h.Bool(eof)
	h.Attr(attrOf(fi))
	return proto.OK, buf[:head+n]
}

func (a *agent) fsync(path string) (proto.Status, []byte) {
	if e := a.fds.peek(path); e != nil {
		err := e.f.Sync()
		a.fds.release(e)
		if err != nil {
			return fail(err)
		}
		return proto.OK, nil
	}
	fi, err := os.Stat(path)
	if err != nil {
		return fail(err)
	}
	if !fi.Mode().IsRegular() {
		return proto.OK, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return fail(err)
	}
	err = f.Sync()
	f.Close()
	if err != nil && !errors.Is(err, syscall.EINVAL) {
		return fail(err)
	}
	return proto.OK, nil
}

// ---------------------------------------------------------------- fd cache

// fdCache keeps recently used files open so that a stream of 1 MiB reads or
// writes does not pay for open/close every time (expensive on Android's
// emulated storage). Entries are purely an optimisation: the host never
// refers to them.
type fdEntry struct {
	path     string
	f        *os.File
	writable bool
	last     time.Time
	refs     int
	dead     bool
	wmu      sync.Mutex // serialises writes so pre/post attributes pair up
}

type fdCache struct {
	mu sync.Mutex
	m  map[string]*fdEntry
}

func (c *fdCache) acquire(path string, write bool) (*fdEntry, error) {
	c.mu.Lock()
	if e := c.m[path]; e != nil && (e.writable || !write) {
		e.refs++
		c.mu.Unlock()
		return e, nil
	}
	c.mu.Unlock()

	// Never open anything but regular files: opening a FIFO blocks and
	// opening a device node can have side effects.
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.IsDir() {
		return nil, syscall.EISDIR
	}
	if !fi.Mode().IsRegular() {
		return nil, syscall.EINVAL
	}
	flag := os.O_RDONLY
	if write {
		flag = os.O_RDWR
	}
	f, err := os.OpenFile(path, flag, 0)
	if err != nil {
		return nil, err
	}
	e := &fdEntry{path: path, f: f, writable: write, refs: 1, last: time.Now()}
	c.mu.Lock()
	defer c.mu.Unlock()
	if old := c.m[path]; old != nil {
		if old.writable || !write { // lost a race; share the winner
			old.refs++
			f.Close()
			return old, nil
		}
		c.dropLocked(old)
	}
	c.m[path] = e
	c.evictLocked()
	return e, nil
}

// peek returns a referenced entry only if one is already cached.
func (c *fdCache) peek(path string) *fdEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.m[path]
	if e != nil {
		e.refs++
	}
	return e
}

func (c *fdCache) put(path string, f *os.File, writable bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if old := c.m[path]; old != nil {
		c.dropLocked(old)
	}
	c.m[path] = &fdEntry{path: path, f: f, writable: writable, last: time.Now()}
	c.evictLocked()
}

func (c *fdCache) release(e *fdEntry) {
	c.mu.Lock()
	e.refs--
	e.last = time.Now()
	closeNow := e.dead && e.refs == 0
	c.mu.Unlock()
	if closeNow {
		e.f.Close()
	}
}

func (c *fdCache) dropLocked(e *fdEntry) {
	if c.m[e.path] == e {
		delete(c.m, e.path)
	}
	e.dead = true
	if e.refs == 0 {
		e.f.Close()
	}
}

// invalidate forgets path and everything below it.
func (c *fdCache) invalidate(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	prefix := path + "/"
	for p, e := range c.m {
		if p == path || (len(p) > len(prefix) && p[:len(prefix)] == prefix) {
			c.dropLocked(e)
		}
	}
}

func (c *fdCache) sweep() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for _, e := range c.m {
		if e.refs == 0 && now.Sub(e.last) > fdIdleClose {
			c.dropLocked(e)
		}
	}
}

func (c *fdCache) evictLocked() {
	for len(c.m) > fdCacheMax {
		var oldest *fdEntry
		for _, e := range c.m {
			if e.refs == 0 && (oldest == nil || e.last.Before(oldest.last)) {
				oldest = e
			}
		}
		if oldest == nil {
			return
		}
		c.dropLocked(oldest)
	}
}
