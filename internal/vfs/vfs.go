// Package vfs sits between the NFS server and the device backend. It owns
// the file-handle table (handles are host-side ids mapped to paths, so they
// survive a device reconnect), short-lived attribute and directory caches,
// and an in-memory home for the metadata files macOS likes to scatter
// around (._*, .DS_Store) so they never reach the device.
//
// Nothing here blocks beyond the backend's own time bounds, and no backend
// call is made while holding the lock.
package vfs

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/Foxhind/macdroidfs/internal/proto"
)

// Backend is the device. All paths are relative to the exported root ("" is
// the root itself). Implementations must bound every call in time.
type Backend interface {
	Stat(rel string) (proto.Attr, error)
	ReadDir(rel string) ([]proto.DirEnt, error)
	Read(rel string, off uint64, n uint32) (data []byte, eof bool, attr proto.Attr, err error)
	Write(rel string, off uint64, data []byte) (pre, post proto.Attr, err error)
	Create(rel string, mode uint32, excl, trunc bool) (proto.Attr, error)
	Mkdir(rel string, mode uint32) (proto.Attr, error)
	Remove(rel string) error
	Rmdir(rel string) error
	Rename(from, to string) error
	Truncate(rel string, size uint64) error
	Utimes(rel string, sec int64, nsec uint32) error
	Fsync(rel string) error
	Statfs() (proto.Statfs, error)
	Symlink(target, rel string) (proto.Attr, error)
	Readlink(rel string) (string, error)
	Epoch() uint64
}

// Options control write access and cache lifetimes.
type Options struct {
	ReadOnly bool
	// KeepMacMetadata stores ._* and .DS_Store files on the device instead
	// of in memory.
	KeepMacMetadata bool

	AttrTTL  time.Duration // how long a stat result is trusted
	NegTTL   time.Duration // how long "does not exist" is trusted
	DirFresh time.Duration // listing age below which it answers lookups
	DirKeep  time.Duration // how long an unused listing is kept for paging
	DirWait  time.Duration // wait for a listing before answering "try later"
}

// DefaultOptions returns the values used in production.
func DefaultOptions() Options {
	return Options{
		AttrTTL:  time.Second,
		NegTTL:   time.Second,
		DirFresh: 1500 * time.Millisecond,
		DirKeep:  30 * time.Second,
		DirWait:  8 * time.Second,
	}
}

const (
	RootID         = 1
	maxNodes       = 1 << 20
	shadowMaxFile  = 16 << 20
	shadowMaxTotal = 64 << 20
)

// Attr is a file's attributes as presented to the client.
type Attr struct {
	FileID uint64
	proto.Attr
}

// DirEntry is one entry of a Listing.
type DirEntry struct {
	Name string
	Attr Attr
}

// Listing is a consistent view of a directory. Entries does not include
// "." and "..", which are provided separately.
type Listing struct {
	Verf        uint64
	Dot, DotDot Attr
	Entries     []DirEntry
}

type attrEnt struct {
	a   proto.Attr
	exp time.Time
}

type dirSnap struct {
	verf    uint64
	fetched time.Time // zero once a mutation made it unfit for lookups
	used    time.Time
	served  bool
	ents    []DirEntry
	byName  map[string]int
}

type dirCall struct {
	done chan struct{}
	snap *dirSnap
	st   proto.Status
}

type shadowFile struct {
	data  []byte
	mtime time.Time
}

type exclEnt struct {
	verf [8]byte
	exp  time.Time
}

// FS maps NFS file IDs to device paths and caches attributes and listings
// in front of a Backend. It is safe for concurrent use.
type FS struct {
	be       Backend
	opt      Options
	instance uint64

	mu          sync.Mutex
	nodes       map[uint64]string
	ids         map[string]uint64
	nextID      uint64
	gen         uint64 // bumped by every mutation; guards cache fills
	attrs       map[string]attrEnt
	neg         map[string]time.Time
	dirs        map[string]*dirSnap
	flight      map[string]*dirCall
	snapSeq     uint64
	shadow      map[string]*shadowFile
	shadowBytes int
	excl        map[string]exclEnt
	fsstat      proto.Statfs
	fsstatExp   time.Time

	stop chan struct{}
}

// New returns an FS over be. Call Close to stop its background cleanup.
func New(be Backend, opt Options) *FS {
	var b [8]byte
	rand.Read(b[:])
	fs := &FS{
		be:       be,
		opt:      opt,
		instance: binary.BigEndian.Uint64(b[:]),
		nodes:    map[uint64]string{RootID: ""},
		ids:      map[string]uint64{"": RootID},
		nextID:   RootID,
		attrs:    map[string]attrEnt{},
		neg:      map[string]time.Time{},
		dirs:     map[string]*dirSnap{},
		flight:   map[string]*dirCall{},
		shadow:   map[string]*shadowFile{},
		excl:     map[string]exclEnt{},
		stop:     make(chan struct{}),
	}
	go fs.janitor()
	return fs
}

// Close stops the background cache cleanup.
func (fs *FS) Close() { close(fs.stop) }

// Instance identifies this FS object; it is part of every file handle.
func (fs *FS) Instance() uint64 { return fs.instance }

// ReadOnly reports whether writes are refused.
func (fs *FS) ReadOnly() bool { return fs.opt.ReadOnly }

// WriteVerf changes whenever unstable writes may have been lost, i.e. on
// every device reconnect (the device may have rebooted in between).
func (fs *FS) WriteVerf() uint64 { return fs.instance ^ (fs.be.Epoch() * 0x9E3779B97F4A7C15) }

// ----------------------------------------------------------------- helpers

func join(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

func parent(rel string) string {
	if i := strings.LastIndexByte(rel, '/'); i >= 0 {
		return rel[:i]
	}
	return ""
}

func base(rel string) string {
	return rel[strings.LastIndexByte(rel, '/')+1:]
}

func under(p, dir string) bool {
	return len(p) > len(dir) && p[len(dir)] == '/' && p[:len(dir)] == dir
}

func validName(name string) proto.Status {
	switch {
	case name == "" || name == "." || name == "..":
		return proto.ErrInval
	case len(name) > 255:
		return proto.ErrNameTooLong
	case strings.ContainsAny(name, "/\x00"):
		return proto.ErrInval
	}
	return proto.OK
}

func isShadowName(name string) bool {
	return strings.HasPrefix(name, "._") || name == ".DS_Store"
}

func (fs *FS) useShadow(name string) bool {
	return !fs.opt.KeepMacMetadata && isShadowName(name)
}

func status(err error) proto.Status {
	if err == nil {
		return proto.OK
	}
	var st proto.Status
	if errors.As(err, &st) {
		return st
	}
	return proto.ErrIO // offline, timeout, broken stream
}

func (sh *shadowFile) attr() proto.Attr {
	return proto.Attr{
		Mode:      proto.ModeReg | 0o644,
		Nlink:     1,
		Size:      uint64(len(sh.data)),
		Used:      uint64(len(sh.data)),
		MtimeSec:  sh.mtime.Unix(),
		MtimeNsec: uint32(sh.mtime.Nanosecond()),
	}
}

// present turns device attributes into what the Mac sees: everything is
// owned by the mounting user, and the real access check happens on the
// device when an operation is attempted.
func (fs *FS) present(id uint64, a proto.Attr) Attr {
	typ := a.Mode & proto.ModeFmt
	perm := uint32(0o644)
	switch {
	case typ == proto.ModeDir:
		perm = 0o755
	case typ == proto.ModeLnk:
		perm = 0o777
	case a.Mode&0o111 != 0:
		perm = 0o755
	}
	if fs.opt.ReadOnly {
		perm &^= 0o222
	}
	a.Mode = typ | perm
	if a.Nlink == 0 {
		a.Nlink = 1
	}
	return Attr{FileID: id, Attr: a}
}

func (fs *FS) pathOf(id uint64) (string, bool) {
	fs.mu.Lock()
	p, ok := fs.nodes[id]
	fs.mu.Unlock()
	return p, ok
}

func (fs *FS) idForLocked(rel string) uint64 {
	if id, ok := fs.ids[rel]; ok {
		return id
	}
	if len(fs.ids) >= maxNodes {
		// Forget an arbitrary tenth; clients re-look-up stale handles.
		n := maxNodes / 10
		for p, id := range fs.ids {
			if id == RootID {
				continue
			}
			delete(fs.ids, p)
			delete(fs.nodes, id)
			if n--; n == 0 {
				break
			}
		}
	}
	fs.nextID++
	fs.ids[rel] = fs.nextID
	fs.nodes[fs.nextID] = rel
	return fs.nextID
}

func (fs *FS) idFor(rel string) uint64 {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.idForLocked(rel)
}

func (fs *FS) dropNodeLocked(rel string) {
	if id, ok := fs.ids[rel]; ok && id != RootID {
		delete(fs.ids, rel)
		delete(fs.nodes, id)
	}
}

// touchDirLocked records that a directory's contents changed.
func (fs *FS) touchDirLocked(dir string) {
	if s := fs.dirs[dir]; s != nil {
		s.fetched = time.Time{}
	}
	delete(fs.attrs, dir)
}

// goneLocked records that rel no longer exists.
func (fs *FS) goneLocked(rel string) {
	fs.gen++
	delete(fs.attrs, rel)
	fs.neg[rel] = time.Now().Add(fs.opt.NegTTL)
	fs.touchDirLocked(parent(rel))
	fs.dropNodeLocked(rel)
}

// bornLocked records that rel now exists with the given attributes.
func (fs *FS) bornLocked(rel string, a proto.Attr) uint64 {
	fs.gen++
	fs.attrs[rel] = attrEnt{a, time.Now().Add(fs.opt.AttrTTL)}
	delete(fs.neg, rel)
	fs.touchDirLocked(parent(rel))
	return fs.idForLocked(rel)
}

func (fs *FS) janitor() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-fs.stop:
			return
		case now := <-t.C:
			fs.mu.Lock()
			for p, e := range fs.attrs {
				if now.After(e.exp) {
					delete(fs.attrs, p)
				}
			}
			for p, exp := range fs.neg {
				if now.After(exp) {
					delete(fs.neg, p)
				}
			}
			for p, s := range fs.dirs {
				if now.Sub(s.used) > fs.opt.DirKeep {
					delete(fs.dirs, p)
				}
			}
			for p, e := range fs.excl {
				if now.After(e.exp) {
					delete(fs.excl, p)
				}
			}
			fs.mu.Unlock()
		}
	}
}

// stat returns the raw attributes of a path, from cache when possible.
func (fs *FS) stat(rel string) (proto.Attr, proto.Status) {
	now := time.Now()
	fs.mu.Lock()
	if sh := fs.shadow[rel]; sh != nil {
		a := sh.attr()
		fs.mu.Unlock()
		return a, proto.OK
	}
	if e, ok := fs.attrs[rel]; ok && now.Before(e.exp) {
		fs.mu.Unlock()
		return e.a, proto.OK
	}
	if exp, ok := fs.neg[rel]; ok && now.Before(exp) {
		fs.mu.Unlock()
		return proto.Attr{}, proto.ErrNoEnt
	}
	if rel != "" {
		if s := fs.dirs[parent(rel)]; s != nil && now.Sub(s.fetched) < fs.opt.DirFresh {
			defer fs.mu.Unlock()
			if i, ok := s.byName[base(rel)]; ok {
				return s.ents[i].Attr.Attr, proto.OK
			}
			return proto.Attr{}, proto.ErrNoEnt
		}
	}
	gen := fs.gen
	fs.mu.Unlock()

	a, err := fs.be.Stat(rel)

	fs.mu.Lock()
	defer fs.mu.Unlock()
	if err != nil {
		st := status(err)
		if st == proto.ErrNoEnt && gen == fs.gen {
			fs.neg[rel] = time.Now().Add(fs.opt.NegTTL)
			delete(fs.attrs, rel)
		}
		return proto.Attr{}, st
	}
	if gen == fs.gen {
		fs.attrs[rel] = attrEnt{a, time.Now().Add(fs.opt.AttrTTL)}
		delete(fs.neg, rel)
	}
	return a, proto.OK
}

// cached returns still-valid attributes without touching the device.
func (fs *FS) cached(id uint64) (Attr, bool) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	rel, ok := fs.nodes[id]
	if !ok {
		return Attr{}, false
	}
	if sh := fs.shadow[rel]; sh != nil {
		return fs.present(id, sh.attr()), true
	}
	if e, ok := fs.attrs[rel]; ok && time.Now().Before(e.exp) {
		return fs.present(id, e.a), true
	}
	return Attr{}, false
}

// CachedAttr is used for the optional pre/post-operation attributes of NFS
// replies, where "unknown" is an acceptable answer.
func (fs *FS) CachedAttr(id uint64) (Attr, bool) { return fs.cached(id) }

// --------------------------------------------------------------- operations

// GetAttr returns the current attributes of file id.
func (fs *FS) GetAttr(id uint64) (Attr, proto.Status) {
	rel, ok := fs.pathOf(id)
	if !ok {
		return Attr{}, proto.ErrStale
	}
	a, st := fs.stat(rel)
	if st != proto.OK {
		if st == proto.ErrNoEnt || st == proto.ErrNotDir {
			if id == RootID {
				return Attr{}, proto.ErrIO
			}
			st = proto.ErrStale
		}
		return Attr{}, st
	}
	return fs.present(id, a), proto.OK
}

// Lookup finds name in directory dir.
func (fs *FS) Lookup(dir uint64, name string) (Attr, proto.Status) {
	drel, ok := fs.pathOf(dir)
	if !ok {
		return Attr{}, proto.ErrStale
	}
	switch name {
	case ".":
		return fs.GetAttr(dir)
	case "..":
		return fs.GetAttr(fs.idFor(parent(drel)))
	}
	if st := validName(name); st != proto.OK {
		return Attr{}, st
	}
	rel := join(drel, name)
	a, st := fs.stat(rel)
	if st != proto.OK {
		return Attr{}, st
	}
	return fs.present(fs.idFor(rel), a), proto.OK
}

func (fs *FS) fetchDir(rel string, c *dirCall, gen uint64) {
	ents, err := fs.be.ReadDir(rel)
	fs.mu.Lock()
	delete(fs.flight, rel)
	if err != nil {
		c.st = status(err)
	} else {
		now := time.Now()
		fs.snapSeq++
		s := &dirSnap{
			verf:   fs.instance ^ fs.snapSeq,
			used:   now,
			ents:   make([]DirEntry, 0, len(ents)),
			byName: make(map[string]int, len(ents)),
		}
		if gen == fs.gen {
			s.fetched = now
		}
		for _, e := range ents {
			if e.Name == "." || e.Name == ".." || validName(e.Name) != proto.OK {
				continue
			}
			child := join(rel, e.Name)
			if fs.shadow[child] != nil {
				continue
			}
			s.byName[e.Name] = len(s.ents)
			s.ents = append(s.ents, DirEntry{Name: e.Name, Attr: fs.present(fs.idForLocked(child), e.Attr)})
		}
		fs.dirs[rel] = s
		c.snap = s
	}
	fs.mu.Unlock()
	close(c.done)
}

func (fs *FS) snapshot(rel string, fresh bool) (*dirSnap, proto.Status) {
	now := time.Now()
	fs.mu.Lock()
	if s := fs.dirs[rel]; s != nil && (!fresh || !s.served || now.Sub(s.fetched) < fs.opt.DirFresh) {
		s.used, s.served = now, true
		fs.mu.Unlock()
		return s, proto.OK
	}
	c := fs.flight[rel]
	if c == nil {
		c = &dirCall{done: make(chan struct{})}
		fs.flight[rel] = c
		go fs.fetchDir(rel, c, fs.gen)
	}
	fs.mu.Unlock()

	// A huge directory on slow storage can take a long time. Rather than
	// keep the client waiting, tell it to come back; the fetch carries on
	// and its result is handed to the retry.
	t := time.NewTimer(fs.opt.DirWait)
	defer t.Stop()
	select {
	case <-c.done:
		if c.st != proto.OK {
			return nil, c.st
		}
		fs.mu.Lock()
		c.snap.served = true
		fs.mu.Unlock()
		return c.snap, proto.OK
	case <-t.C:
		return nil, proto.ErrJukebox
	}
}

// ReadDir returns the listing to page through. A zero cookie starts a new
// pass (and refreshes the listing if it is old); a non-zero cookie continues
// on the listing already held so that positions stay stable.
func (fs *FS) ReadDir(dir uint64, cookie uint64) (*Listing, proto.Status) {
	rel, ok := fs.pathOf(dir)
	if !ok {
		return nil, proto.ErrStale
	}
	dot, st := fs.GetAttr(dir)
	if st != proto.OK {
		return nil, st
	}
	if !dot.IsDir() {
		return nil, proto.ErrNotDir
	}
	dotdot := dot
	if rel != "" {
		if a, st := fs.GetAttr(fs.idFor(parent(rel))); st == proto.OK {
			dotdot = a
		}
	}
	s, st := fs.snapshot(rel, cookie == 0)
	if st != proto.OK {
		if st == proto.ErrNoEnt {
			st = proto.ErrStale
		}
		return nil, st
	}
	return &Listing{Verf: s.verf, Dot: dot, DotDot: dotdot, Entries: s.ents}, proto.OK
}

// Read reads up to count bytes of file id at off and reports end of file.
func (fs *FS) Read(id uint64, off uint64, count uint32) ([]byte, bool, proto.Status) {
	rel, ok := fs.pathOf(id)
	if !ok {
		return nil, false, proto.ErrStale
	}
	if count > proto.MaxIO {
		count = proto.MaxIO
	}
	fs.mu.Lock()
	if sh := fs.shadow[rel]; sh != nil {
		defer fs.mu.Unlock()
		if off >= uint64(len(sh.data)) {
			return nil, true, proto.OK
		}
		end := min(off+uint64(count), uint64(len(sh.data)))
		out := append([]byte(nil), sh.data[off:end]...)
		return out, end == uint64(len(sh.data)), proto.OK
	}
	gen := fs.gen
	fs.mu.Unlock()

	data, eof, a, err := fs.be.Read(rel, off, count)
	if err != nil {
		st := status(err)
		if st == proto.ErrNoEnt {
			st = proto.ErrStale
		}
		return nil, false, st
	}
	fs.mu.Lock()
	if e, ok := fs.attrs[rel]; gen == fs.gen && (!ok || time.Now().After(e.exp)) {
		fs.attrs[rel] = attrEnt{a, time.Now().Add(fs.opt.AttrTTL)}
	}
	fs.mu.Unlock()
	return data, eof, proto.OK
}

// Write returns the attributes from just before and just after the write
// (the NFS "weak cache consistency" data).
func (fs *FS) Write(id uint64, off uint64, data []byte) (pre, post Attr, st proto.Status) {
	if fs.opt.ReadOnly {
		return pre, post, proto.ErrROFS
	}
	rel, ok := fs.pathOf(id)
	if !ok {
		return pre, post, proto.ErrStale
	}
	fs.mu.Lock()
	if sh := fs.shadow[rel]; sh != nil {
		defer fs.mu.Unlock()
		pre = fs.present(id, sh.attr())
		end := off + uint64(len(data))
		if end > shadowMaxFile {
			return pre, pre, proto.ErrFBig
		}
		if grow := int(end) - len(sh.data); grow > 0 {
			if fs.shadowBytes+grow > shadowMaxTotal {
				return pre, pre, proto.ErrNoSpc
			}
			sh.data = append(sh.data, make([]byte, grow)...)
			fs.shadowBytes += grow
		}
		copy(sh.data[off:], data)
		sh.mtime = time.Now()
		return pre, fs.present(id, sh.attr()), proto.OK
	}
	fs.mu.Unlock()

	rawPre, rawPost, err := fs.be.Write(rel, off, data)
	if err != nil {
		st := status(err)
		if st == proto.ErrNoEnt {
			st = proto.ErrStale
		}
		return pre, post, st
	}
	fs.mu.Lock()
	fs.gen++
	// Replies to parallel writes arrive in any order; never let the cached
	// attributes move backwards because of that.
	if e, ok := fs.attrs[rel]; !ok || time.Now().After(e.exp) || e.a.Size <= rawPost.Size {
		fs.attrs[rel] = attrEnt{rawPost, time.Now().Add(fs.opt.AttrTTL)}
	}
	if s := fs.dirs[parent(rel)]; s != nil {
		s.fetched = time.Time{}
	}
	fs.mu.Unlock()
	return fs.present(id, rawPre), fs.present(id, rawPost), proto.OK
}

// Commit makes earlier writes to file id durable on the device.
func (fs *FS) Commit(id uint64) proto.Status {
	rel, ok := fs.pathOf(id)
	if !ok {
		return proto.ErrStale
	}
	fs.mu.Lock()
	sh := fs.shadow[rel]
	fs.mu.Unlock()
	if sh != nil || fs.opt.ReadOnly {
		return proto.OK
	}
	st := status(fs.be.Fsync(rel))
	if st == proto.ErrNoEnt {
		st = proto.ErrStale
	}
	return st
}

// exclReplay reports whether an exclusive create that found the file
// already there is a retransmission of the create that made it.
func (fs *FS) exclReplayLocked(rel string, verf *[8]byte) bool {
	e, ok := fs.excl[rel]
	return verf != nil && ok && e.verf == *verf && time.Now().Before(e.exp)
}

// Create makes a regular file. verf is the NFS exclusive-create verifier,
// if any.
func (fs *FS) Create(dir uint64, name string, mode uint32, excl, trunc bool, verf *[8]byte) (Attr, proto.Status) {
	if fs.opt.ReadOnly {
		return Attr{}, proto.ErrROFS
	}
	drel, ok := fs.pathOf(dir)
	if !ok {
		return Attr{}, proto.ErrStale
	}
	if st := validName(name); st != proto.OK {
		return Attr{}, st
	}
	rel := join(drel, name)

	if fs.useShadow(name) {
		shadowHit := func() (Attr, proto.Status, bool) {
			fs.mu.Lock()
			defer fs.mu.Unlock()
			sh := fs.shadow[rel]
			if sh == nil {
				return Attr{}, proto.OK, false
			}
			if excl && !fs.exclReplayLocked(rel, verf) {
				return Attr{}, proto.ErrExist, true
			}
			if trunc {
				fs.shadowBytes -= len(sh.data)
				sh.data, sh.mtime = nil, time.Now()
			}
			return fs.present(fs.idForLocked(rel), sh.attr()), proto.OK, true
		}
		if a, st, hit := shadowHit(); hit {
			return a, st
		}
		// Only keep it in memory if there is no real file of that name.
		switch _, st := fs.stat(rel); st {
		case proto.OK:
			// fall through to the device
		case proto.ErrNoEnt:
			fs.mu.Lock()
			defer fs.mu.Unlock()
			sh := fs.shadow[rel]
			if sh == nil {
				sh = &shadowFile{mtime: time.Now()}
				fs.shadow[rel] = sh
			}
			if verf != nil {
				fs.excl[rel] = exclEnt{*verf, time.Now().Add(time.Minute)}
			}
			delete(fs.neg, rel)
			return fs.present(fs.idForLocked(rel), sh.attr()), proto.OK
		default:
			return Attr{}, st
		}
	}

	a, err := fs.be.Create(rel, mode, excl, trunc)
	if err != nil {
		st := status(err)
		if st == proto.ErrExist && excl {
			fs.mu.Lock()
			replay := fs.exclReplayLocked(rel, verf)
			fs.mu.Unlock()
			if replay {
				if a, st := fs.stat(rel); st == proto.OK {
					return fs.present(fs.idFor(rel), a), proto.OK
				}
			}
		}
		return Attr{}, st
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	id := fs.bornLocked(rel, a)
	if verf != nil {
		fs.excl[rel] = exclEnt{*verf, time.Now().Add(time.Minute)}
	}
	return fs.present(id, a), proto.OK
}

// Mkdir creates directory name in dir.
func (fs *FS) Mkdir(dir uint64, name string, mode uint32) (Attr, proto.Status) {
	if fs.opt.ReadOnly {
		return Attr{}, proto.ErrROFS
	}
	drel, ok := fs.pathOf(dir)
	if !ok {
		return Attr{}, proto.ErrStale
	}
	if st := validName(name); st != proto.OK {
		return Attr{}, st
	}
	rel := join(drel, name)
	a, err := fs.be.Mkdir(rel, mode)
	if err != nil {
		return Attr{}, status(err)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.present(fs.bornLocked(rel, a), a), proto.OK
}

// Symlink creates a symbolic link name in dir pointing to target.
func (fs *FS) Symlink(dir uint64, name, target string) (Attr, proto.Status) {
	if fs.opt.ReadOnly {
		return Attr{}, proto.ErrROFS
	}
	drel, ok := fs.pathOf(dir)
	if !ok {
		return Attr{}, proto.ErrStale
	}
	if st := validName(name); st != proto.OK {
		return Attr{}, st
	}
	rel := join(drel, name)
	a, err := fs.be.Symlink(target, rel)
	if err != nil {
		return Attr{}, status(err)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.present(fs.bornLocked(rel, a), a), proto.OK
}

// Readlink returns the target of symbolic link id.
func (fs *FS) Readlink(id uint64) (string, proto.Status) {
	rel, ok := fs.pathOf(id)
	if !ok {
		return "", proto.ErrStale
	}
	s, err := fs.be.Readlink(rel)
	return s, status(err)
}

// Remove deletes file name from dir.
func (fs *FS) Remove(dir uint64, name string) proto.Status {
	if fs.opt.ReadOnly {
		return proto.ErrROFS
	}
	drel, ok := fs.pathOf(dir)
	if !ok {
		return proto.ErrStale
	}
	if st := validName(name); st != proto.OK {
		return st
	}
	rel := join(drel, name)
	fs.mu.Lock()
	if sh := fs.shadow[rel]; sh != nil {
		fs.shadowBytes -= len(sh.data)
		delete(fs.shadow, rel)
		delete(fs.excl, rel)
		fs.dropNodeLocked(rel)
		fs.mu.Unlock()
		return proto.OK
	}
	fs.mu.Unlock()
	if err := fs.be.Remove(rel); err != nil {
		st := status(err)
		if st == proto.ErrNoEnt {
			fs.mu.Lock()
			fs.goneLocked(rel)
			fs.mu.Unlock()
		}
		return st
	}
	fs.mu.Lock()
	fs.goneLocked(rel)
	delete(fs.excl, rel)
	fs.mu.Unlock()
	return proto.OK
}

// Rmdir deletes empty directory name from dir.
func (fs *FS) Rmdir(dir uint64, name string) proto.Status {
	if fs.opt.ReadOnly {
		return proto.ErrROFS
	}
	drel, ok := fs.pathOf(dir)
	if !ok {
		return proto.ErrStale
	}
	if st := validName(name); st != proto.OK {
		return st
	}
	rel := join(drel, name)
	if err := fs.be.Rmdir(rel); err != nil {
		return status(err)
	}
	fs.mu.Lock()
	fs.goneLocked(rel)
	delete(fs.dirs, rel)
	for p, sh := range fs.shadow {
		if under(p, rel) {
			fs.shadowBytes -= len(sh.data)
			delete(fs.shadow, p)
			fs.dropNodeLocked(p)
		}
	}
	fs.mu.Unlock()
	return proto.OK
}

// Rename moves fromName in fromDir to toName in toDir.
func (fs *FS) Rename(fromDir uint64, fromName string, toDir uint64, toName string) proto.Status {
	if fs.opt.ReadOnly {
		return proto.ErrROFS
	}
	fdir, ok1 := fs.pathOf(fromDir)
	tdir, ok2 := fs.pathOf(toDir)
	if !ok1 || !ok2 {
		return proto.ErrStale
	}
	if st := validName(fromName); st != proto.OK {
		return st
	}
	if st := validName(toName); st != proto.OK {
		return st
	}
	from, to := join(fdir, fromName), join(tdir, toName)
	if from == to {
		return proto.OK
	}

	fs.mu.Lock()
	if sh := fs.shadow[from]; sh != nil {
		defer fs.mu.Unlock()
		if !fs.useShadow(toName) {
			return proto.ErrAcces // memory-only files cannot become real ones
		}
		if old := fs.shadow[to]; old != nil {
			fs.shadowBytes -= len(old.data)
		}
		fs.dropNodeLocked(to)
		fs.shadow[to] = sh
		delete(fs.shadow, from)
		fs.moveNodeLocked(from, to)
		return proto.OK
	}
	fromAttr, known := fs.attrs[from]
	fs.mu.Unlock()

	if err := fs.be.Rename(from, to); err != nil {
		return status(err)
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.gen++
	if old := fs.shadow[to]; old != nil {
		fs.shadowBytes -= len(old.data)
		delete(fs.shadow, to)
	}
	fs.dropNodeLocked(to)
	fs.moveNodeLocked(from, to)
	maybeDir := !known || fromAttr.a.IsDir()
	if maybeDir {
		for p, id := range fs.ids {
			if under(p, from) {
				np := to + p[len(from):]
				delete(fs.ids, p)
				fs.ids[np] = id
				fs.nodes[id] = np
			}
		}
		for p, sh := range fs.shadow {
			if under(p, from) {
				delete(fs.shadow, p)
				fs.shadow[to+p[len(from):]] = sh
			}
		}
		for p := range fs.attrs {
			if under(p, from) || under(p, to) {
				delete(fs.attrs, p)
			}
		}
		for p := range fs.neg {
			if under(p, from) || under(p, to) {
				delete(fs.neg, p)
			}
		}
		for p := range fs.dirs {
			if p == from || p == to || under(p, from) || under(p, to) {
				delete(fs.dirs, p)
			}
		}
	}
	delete(fs.attrs, from)
	delete(fs.attrs, to)
	delete(fs.neg, to)
	fs.neg[from] = time.Now().Add(fs.opt.NegTTL)
	fs.touchDirLocked(fdir)
	fs.touchDirLocked(tdir)
	return proto.OK
}

func (fs *FS) moveNodeLocked(from, to string) {
	if id, ok := fs.ids[from]; ok {
		delete(fs.ids, from)
		fs.ids[to] = id
		fs.nodes[id] = to
	}
}

// SetAttr changes size and/or modification time. Ownership and mode changes
// are accepted and ignored: Android's shared storage does not have them.
func (fs *FS) SetAttr(id uint64, size *uint64, mtime *time.Time) (Attr, proto.Status) {
	rel, ok := fs.pathOf(id)
	if !ok {
		return Attr{}, proto.ErrStale
	}
	if size == nil && mtime == nil {
		return fs.GetAttr(id)
	}
	if fs.opt.ReadOnly {
		return Attr{}, proto.ErrROFS
	}
	fs.mu.Lock()
	if sh := fs.shadow[rel]; sh != nil {
		defer fs.mu.Unlock()
		if size != nil {
			if *size > shadowMaxFile {
				return Attr{}, proto.ErrFBig
			}
			n := int(*size)
			fs.shadowBytes += n - len(sh.data)
			if n <= len(sh.data) {
				sh.data = sh.data[:n]
			} else {
				sh.data = append(sh.data, make([]byte, n-len(sh.data))...)
			}
			sh.mtime = time.Now()
		}
		if mtime != nil {
			sh.mtime = *mtime
		}
		return fs.present(id, sh.attr()), proto.OK
	}
	fs.mu.Unlock()

	if size != nil {
		if err := fs.be.Truncate(rel, *size); err != nil {
			st := status(err)
			if st == proto.ErrNoEnt {
				st = proto.ErrStale
			}
			return Attr{}, st
		}
	}
	if mtime != nil {
		// Emulated storage often refuses utimes; that must not fail a copy.
		if err := fs.be.Utimes(rel, mtime.Unix(), uint32(mtime.Nanosecond())); err != nil {
			switch st := status(err); st {
			case proto.ErrPerm, proto.ErrAcces, proto.ErrNotSupp, proto.ErrInval, proto.ErrROFS:
			case proto.ErrNoEnt:
				return Attr{}, proto.ErrStale
			default:
				return Attr{}, st
			}
		}
	}
	fs.mu.Lock()
	fs.gen++
	delete(fs.attrs, rel)
	if s := fs.dirs[parent(rel)]; s != nil {
		s.fetched = time.Time{}
	}
	fs.mu.Unlock()
	return fs.GetAttr(id)
}

// Statfs returns file system usage, cached briefly.
func (fs *FS) Statfs() (proto.Statfs, proto.Status) {
	fs.mu.Lock()
	if time.Now().Before(fs.fsstatExp) {
		s := fs.fsstat
		fs.mu.Unlock()
		return s, proto.OK
	}
	fs.mu.Unlock()
	s, err := fs.be.Statfs()
	if err != nil {
		return proto.Statfs{}, status(err)
	}
	fs.mu.Lock()
	fs.fsstat, fs.fsstatExp = s, time.Now().Add(2*time.Second)
	fs.mu.Unlock()
	return s, proto.OK
}
