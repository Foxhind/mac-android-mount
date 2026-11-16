// Package mtpfs turns an MTP session into the path-based file system
// backend the rest of droidfs works with (the same interface the adb agent
// provides), so that a device without USB debugging can be mounted too.
//
// MTP addresses objects by numeric handle and has no path lookup, so this
// package keeps a cache of directory listings. Writing in place relies on
// Android's edit extension (begin edit / send partial object / truncate /
// end edit); a device without it is exposed read-only.
package mtpfs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"droidfs/internal/mtp"
	"droidfs/internal/proto"
)

// ErrNoStorage means the device answered but offers no storage, which is
// what a locked phone (or one still in "charging only" mode) does.
var ErrNoStorage = errors.New("no storage visible: unlock the device and choose \"File transfer\" for USB")

// OpenFunc opens the USB transport of the device.
type OpenFunc func(ctx context.Context) (mtp.Transport, error)

type Options struct {
	MetaTimeout time.Duration // lookups, create, rename, ...
	DataTimeout time.Duration // one read or write
	ListTimeout time.Duration // a whole directory listing
	DirTTL      time.Duration // how often a folder's object list is re-read
	InfoTTL     time.Duration // how long a file's size/date is trusted
	EditIdle    time.Duration // close an edit after this long without writes
	MTP         mtp.Options
}

func DefaultOptions() Options {
	return Options{
		MetaTimeout: 12 * time.Second,
		DataTimeout: 20 * time.Second,
		ListTimeout: 120 * time.Second,
		DirTTL:      3 * time.Second,
		InfoTTL:     2 * time.Second,
		EditIdle:    3 * time.Second,
		MTP:         mtp.DefaultOptions(),
	}
}

type node struct {
	storage, handle uint32
	name            string
	dir             bool
	virtual         bool // the synthetic root above several storages
	size            uint64
	mtime           time.Time
	infoAt          time.Time
}

type dirKey struct{ storage, handle uint32 }

type dirCache struct {
	fetched time.Time
	// changed is when the set of children was last seen to differ. MTP
	// folders (and above all storage roots) have no modification time that
	// moves when their contents change, yet the Mac's NFS client relies on
	// exactly that to drop cached listings and cached "no such file"
	// answers. So one is synthesised.
	changed time.Time
	nodes   []*node
	byName  map[string]*node
}

type loadCall struct {
	done chan struct{}
	dc   *dirCache
	err  error
}

type edit struct {
	refs int
	last time.Time
}

// override holds what we know better than the device about a file we are
// writing: old Android releases only refresh an object's size once the edit
// is finished and the file has been rescanned.
type override struct {
	size  uint64
	mtime time.Time
	until time.Time // zero while the edit is open
}

type store struct {
	id   uint32
	name string
	root *node
}

type session struct {
	conn     *mtp.Conn
	opt      Options
	stores   []store
	multi    bool
	writable bool
	down     chan struct{}
	stop     chan struct{}
	dropOnce sync.Once

	editMu sync.Mutex // serialises begin/end edit

	mu       sync.Mutex
	dirs     map[dirKey]*dirCache
	loading  map[dirKey]*loadCall
	byHandle map[uint32]*node
	edits    map[uint32]*edit
	over     map[uint32]*override
}

// Backend is a reconnectable MTP device.
type Backend struct {
	open OpenFunc
	opt  Options

	mu    sync.RWMutex
	cur   *session
	epoch atomic.Uint64
	desc  string
}

func New(open OpenFunc, opt Options) *Backend { return &Backend{open: open, opt: opt} }

// ------------------------------------------------------------ connection

func (b *Backend) Connect(ctx context.Context) error {
	b.Disconnect()
	t, err := b.open(ctx)
	if err != nil {
		return err
	}
	s := &session{
		opt:      b.opt,
		down:     make(chan struct{}),
		stop:     make(chan struct{}),
		dirs:     map[dirKey]*dirCache{},
		loading:  map[dirKey]*loadCall{},
		byHandle: map[uint32]*node{},
		edits:    map[uint32]*edit{},
		over:     map[uint32]*override{},
	}
	conn, err := mtp.Open(ctx, t, b.opt.MTP, s.onEvent)
	if err != nil {
		return fmt.Errorf("open MTP session: %w", err)
	}
	s.conn = conn
	s.writable = conn.CanEdit()
	ids, err := conn.GetStorageIDs(ctx)
	if err == nil && len(ids) == 0 {
		err = ErrNoStorage
	}
	if err != nil {
		conn.Close()
		return err
	}
	used := map[string]bool{}
	for _, id := range ids {
		si, err := conn.GetStorageInfo(ctx, id)
		if err != nil {
			continue
		}
		name := strings.Map(func(r rune) rune {
			if r == '/' || r < 0x20 {
				return '-'
			}
			return r
		}, strings.TrimSpace(si.Description))
		if name == "" {
			name = fmt.Sprintf("Storage %08X", id)
		}
		for used[name] {
			name += " 2"
		}
		used[name] = true
		s.stores = append(s.stores, store{id: id, name: name,
			root: &node{storage: id, handle: mtp.RootParent, name: name, dir: true}})
	}
	if len(s.stores) == 0 {
		conn.Close()
		return ErrNoStorage
	}
	s.multi = len(s.stores) > 1
	names := make([]string, len(s.stores))
	for i, st := range s.stores {
		names[i] = st.name
	}
	desc := "MTP: " + strings.Join(names, ", ")
	if !s.writable {
		desc += " (read-only: device lacks the Android edit extension)"
	}

	b.mu.Lock()
	b.cur, b.desc = s, desc
	b.epoch.Add(1)
	b.mu.Unlock()
	go s.sweeper()
	go func() {
		<-conn.Dead()
		b.drop(s)
	}()
	return nil
}

func (b *Backend) drop(s *session) {
	b.mu.Lock()
	if b.cur == s {
		b.cur = nil
	}
	b.mu.Unlock()
	s.dropOnce.Do(func() {
		close(s.stop)
		s.conn.Close()
		close(s.down)
	})
}

func (b *Backend) Disconnect() {
	b.mu.RLock()
	s := b.cur
	b.mu.RUnlock()
	if s != nil {
		b.drop(s)
	}
}

func (b *Backend) Down() <-chan struct{} {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.cur == nil {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	return b.cur.down
}

func (b *Backend) Online() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.cur != nil
}

// Root describes what is exported (for logs).
func (b *Backend) Root() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.desc
}

func (b *Backend) Epoch() uint64 { return b.epoch.Load() }

func (b *Backend) sess() (*session, error) {
	b.mu.RLock()
	s := b.cur
	b.mu.RUnlock()
	if s == nil {
		return nil, mtp.ErrOffline
	}
	return s, nil
}

// --------------------------------------------------------------- helpers

// xlate turns device responses into the status codes the VFS understands.
// Transport-level failures pass through and end up as EIO.
func xlate(err error) error {
	var re mtp.RespError
	if !errors.As(err, &re) {
		return err
	}
	switch uint16(re) {
	case mtp.RespInvalidObjectHandle, mtp.RespInvalidStorageID, mtp.RespInvalidParentObject, mtp.RespStoreNotAvailable:
		return proto.ErrNoEnt
	case mtp.RespStoreFull, mtp.RespObjectTooLarge:
		return proto.ErrNoSpc
	case mtp.RespStoreReadOnly:
		return proto.ErrROFS
	case mtp.RespObjectWriteProtected, mtp.RespAccessDenied:
		return proto.ErrAcces
	case mtp.RespOperationNotSupp, mtp.RespParameterNotSupp, mtp.RespSpecByFormatUnsupp:
		return proto.ErrNotSupp
	case mtp.RespInvalidParameter, mtp.RespInvalidObjectPropVal:
		return proto.ErrInval
	}
	return proto.ErrIO
}

func isResp(err error, code uint16) bool {
	var re mtp.RespError
	return errors.As(err, &re) && uint16(re) == code
}

func (s *session) meta() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), s.opt.MetaTimeout)
}

func (s *session) data() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), s.opt.DataTimeout)
}

func splitRel(rel string) (dir, name string) {
	if i := strings.LastIndexByte(rel, '/'); i >= 0 {
		return rel[:i], rel[i+1:]
	}
	return "", rel
}

var virtualRoot = &node{dir: true, virtual: true}

// attr is called with s.mu held.
func (s *session) attrLocked(n *node) proto.Attr {
	a := proto.Attr{Nlink: 1, Ino: uint64(n.handle), MtimeSec: n.mtime.Unix(), MtimeNsec: uint32(n.mtime.Nanosecond())}
	if n.mtime.IsZero() {
		a.MtimeSec, a.MtimeNsec = 0, 0
	}
	if n.dir {
		a.Mode = proto.ModeDir | 0o755
		if dc := s.dirs[dirKey{n.storage, n.handle}]; dc != nil && dc.changed.After(n.mtime) {
			a.MtimeSec, a.MtimeNsec = dc.changed.Unix(), uint32(dc.changed.Nanosecond())
		}
		return a
	}
	a.Mode = proto.ModeReg | 0o644
	a.Size = n.size
	if o := s.over[n.handle]; o != nil {
		a.Size, a.MtimeSec, a.MtimeNsec = o.size, o.mtime.Unix(), uint32(o.mtime.Nanosecond())
	}
	a.Used = a.Size
	return a
}

func (s *session) attr(n *node) proto.Attr {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attrLocked(n)
}

// fillLocked refreshes the changeable part of n from the device's answer
// (s.mu held). A node's name and kind are set once, when it is created.
func (s *session) fillLocked(n *node, oi *mtp.ObjectInfo, size uint64) {
	n.size, n.mtime, n.infoAt = size, oi.Modified, time.Now()
	if o := s.over[n.handle]; o != nil && !o.until.IsZero() && size == o.size {
		delete(s.over, n.handle) // the device has caught up
	}
}

// info fetches an object's information, including its real size when that
// does not fit the 32-bit field.
func (s *session) info(ctx context.Context, h uint32) (mtp.ObjectInfo, uint64, error) {
	oi, err := s.conn.GetObjectInfo(ctx, h)
	if err != nil {
		return oi, 0, err
	}
	size := uint64(oi.Size)
	if oi.Size == 0xFFFFFFFF && !oi.IsDir() {
		if v, err := s.conn.GetObjectSize(ctx, h); err == nil {
			size = v
		}
	}
	return oi, size, nil
}

// children returns the listing of a directory, re-reading the device's
// object list when it is older than DirTTL. Objects already known keep
// their cached information, so a refresh of a big folder is one request.
func (s *session) children(dir *node) (*dirCache, error) {
	key := dirKey{dir.storage, dir.handle}
	s.mu.Lock()
	if dc := s.dirs[key]; dc != nil && time.Since(dc.fetched) < s.opt.DirTTL {
		s.mu.Unlock()
		return dc, nil
	}
	call := s.loading[key]
	if call == nil {
		call = &loadCall{done: make(chan struct{})}
		s.loading[key] = call
		go s.load(key, call)
	}
	s.mu.Unlock()
	select {
	case <-call.done:
		return call.dc, call.err
	case <-s.down:
		return nil, mtp.ErrOffline
	}
}

func (s *session) load(key dirKey, call *loadCall) {
	ctx, cancel := context.WithTimeout(context.Background(), s.opt.ListTimeout)
	defer cancel()
	finish := func(dc *dirCache, err error) {
		s.mu.Lock()
		delete(s.loading, key)
		if err == nil {
			if old := s.dirs[key]; old != nil {
				dc.changed = old.changed
				same := len(old.nodes) == len(dc.nodes)
				for i := 0; same && i < len(dc.nodes); i++ {
					same = old.byName[dc.nodes[i].name] == dc.nodes[i]
				}
				if !same {
					dc.changed = time.Now()
				}
			}
			s.dirs[key] = dc
		}
		s.mu.Unlock()
		call.dc, call.err = dc, err
		close(call.done)
	}
	handles, err := s.conn.GetObjectHandles(ctx, key.storage, key.handle)
	if err != nil {
		finish(nil, xlate(err))
		return
	}
	dc := &dirCache{byName: make(map[string]*node, len(handles)), nodes: make([]*node, 0, len(handles))}
	for _, h := range handles {
		s.mu.Lock()
		n := s.byHandle[h]
		s.mu.Unlock()
		if n == nil {
			oi, size, err := s.info(ctx, h)
			if err != nil {
				if _, gone := err.(mtp.RespError); gone {
					continue // deleted while we were listing
				}
				finish(nil, err)
				return
			}
			n = &node{storage: key.storage, handle: h, name: oi.Name, dir: oi.IsDir()}
			s.mu.Lock()
			s.fillLocked(n, &oi, size)
			s.byHandle[h] = n
			s.mu.Unlock()
		}
		s.mu.Lock()
		name := n.name
		s.mu.Unlock()
		if name == "" || name == "." || name == ".." || strings.ContainsRune(name, '/') || dc.byName[name] != nil {
			continue
		}
		dc.byName[name] = n
		dc.nodes = append(dc.nodes, n)
	}
	dc.fetched = time.Now()
	finish(dc, nil)
}

// resolve walks a relative path to its node.
func (s *session) resolve(rel string) (*node, error) {
	var cur *node
	comps := []string(nil)
	if rel != "" {
		comps = strings.Split(rel, "/")
	}
	if s.multi {
		if len(comps) == 0 {
			return virtualRoot, nil
		}
		for i := range s.stores {
			if s.stores[i].name == comps[0] {
				cur = s.stores[i].root
			}
		}
		if cur == nil {
			return nil, proto.ErrNoEnt
		}
		comps = comps[1:]
	} else {
		cur = s.stores[0].root
	}
	for _, c := range comps {
		if !cur.dir {
			return nil, proto.ErrNotDir
		}
		next, err := s.lookupChild(cur, c)
		if err != nil {
			return nil, err
		}
		cur = next
	}
	return cur, nil
}

// unlinkLocked removes a node from its parent's cached listing.
func (s *session) unlinkLocked(parent, n *node) {
	if dc := s.dirs[dirKey{parent.storage, parent.handle}]; dc != nil && dc.byName[n.name] == n {
		dc.changed = time.Now()
		delete(dc.byName, n.name)
		for i, c := range dc.nodes {
			if c == n {
				dc.nodes = append(dc.nodes[:i:i], dc.nodes[i+1:]...)
				break
			}
		}
	}
}

// forget drops everything known about an object that no longer exists.
func (s *session) forget(parent, n *node) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byHandle, n.handle)
	delete(s.over, n.handle)
	delete(s.dirs, dirKey{n.storage, n.handle})
	s.unlinkLocked(parent, n)
}

// adopt adds a node to a parent's cached listing.
func (s *session) adopt(parent, n *node) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byHandle[n.handle] = n
	if dc := s.dirs[dirKey{parent.storage, parent.handle}]; dc != nil && dc.byName[n.name] == nil {
		dc.changed = time.Now()
		dc.byName[n.name] = n
		dc.nodes = append(dc.nodes, n)
	}
}

func (s *session) onEvent(ev mtp.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch ev.Code {
	case mtp.EvObjectInfoChanged:
		if len(ev.Params) > 0 {
			if n := s.byHandle[ev.Params[0]]; n != nil {
				n.infoAt = time.Time{}
			}
		}
	case mtp.EvObjectAdded, mtp.EvObjectRemoved, mtp.EvStoreAdded, mtp.EvStoreRemoved:
		if ev.Code == mtp.EvObjectRemoved && len(ev.Params) > 0 {
			delete(s.byHandle, ev.Params[0])
		}
		for _, dc := range s.dirs {
			dc.fetched = time.Time{}
		}
	}
}

// ----------------------------------------------------------------- edits

func (s *session) acquireEdit(h uint32) error {
	s.editMu.Lock()
	defer s.editMu.Unlock()
	s.mu.Lock()
	if e := s.edits[h]; e != nil {
		e.refs++
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()
	ctx, cancel := s.meta()
	defer cancel()
	err := s.conn.BeginEdit(ctx, h)
	if isResp(err, mtp.RespGeneralError) {
		// Probably still open from a request we gave up waiting for.
		s.conn.EndEdit(ctx, h)
		err = s.conn.BeginEdit(ctx, h)
	}
	if err != nil {
		return xlate(err)
	}
	s.mu.Lock()
	s.edits[h] = &edit{refs: 1, last: time.Now()}
	s.mu.Unlock()
	return nil
}

func (s *session) releaseEdit(h uint32) {
	s.mu.Lock()
	if e := s.edits[h]; e != nil {
		e.refs--
		e.last = time.Now()
	}
	s.mu.Unlock()
}

// endEdit finishes an edit, which makes the device commit the file and
// re-index it. Unless forced it leaves an edit alone while writes are in
// progress.
func (s *session) endEdit(h uint32, force bool) error {
	s.editMu.Lock()
	defer s.editMu.Unlock()
	s.mu.Lock()
	e := s.edits[h]
	if e == nil || (!force && e.refs > 0) {
		s.mu.Unlock()
		return nil
	}
	delete(s.edits, h)
	if o := s.over[h]; o != nil {
		o.until = time.Now().Add(15 * time.Second)
	}
	if n := s.byHandle[h]; n != nil {
		n.infoAt = time.Time{}
	}
	s.mu.Unlock()
	ctx, cancel := s.meta()
	defer cancel()
	return xlate(s.conn.EndEdit(ctx, h))
}

func (s *session) sweeper() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case now := <-t.C:
			var idle []uint32
			s.mu.Lock()
			for h, e := range s.edits {
				if e.refs == 0 && now.Sub(e.last) > s.opt.EditIdle {
					idle = append(idle, h)
				}
			}
			for h, o := range s.over {
				if !o.until.IsZero() && now.After(o.until) {
					delete(s.over, h)
				}
			}
			s.mu.Unlock()
			for _, h := range idle {
				s.endEdit(h, false)
			}
		}
	}
}

func (s *session) noteWrite(n *node, end uint64, truncate bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.over[n.handle]
	if o == nil {
		o = &override{size: n.size}
		s.over[n.handle] = o
	}
	if truncate || end > o.size {
		o.size = end
	}
	o.mtime, o.until = time.Now(), time.Time{}
}

// ------------------------------------------------------------ operations

func (b *Backend) Stat(rel string) (proto.Attr, error) {
	s, err := b.sess()
	if err != nil {
		return proto.Attr{}, err
	}
	n, err := s.resolve(rel)
	if err != nil {
		return proto.Attr{}, err
	}
	s.mu.Lock()
	stale := !n.dir && time.Since(n.infoAt) > s.opt.InfoTTL
	dc := s.dirs[dirKey{n.storage, n.handle}]
	relist := n.dir && !n.virtual && dc != nil && time.Since(dc.fetched) > s.opt.DirTTL
	s.mu.Unlock()
	if relist {
		// Looking at a folder that has been listed before: check whether
		// its contents changed, so that its modification time tells.
		if _, err := s.children(n); err != nil {
			return proto.Attr{}, err
		}
	}
	if stale {
		ctx, cancel := s.meta()
		defer cancel()
		oi, size, err := s.info(ctx, n.handle)
		if err != nil {
			if isResp(err, mtp.RespInvalidObjectHandle) {
				s.mu.Lock()
				delete(s.byHandle, n.handle)
				for _, dc := range s.dirs {
					dc.fetched = time.Time{}
				}
				s.mu.Unlock()
			}
			return proto.Attr{}, xlate(err)
		}
		s.mu.Lock()
		s.fillLocked(n, &oi, size)
		s.mu.Unlock()
	}
	return s.attr(n), nil
}

func (b *Backend) ReadDir(rel string) ([]proto.DirEnt, error) {
	s, err := b.sess()
	if err != nil {
		return nil, err
	}
	n, err := s.resolve(rel)
	if err != nil {
		return nil, err
	}
	if n.virtual {
		out := make([]proto.DirEnt, len(s.stores))
		for i, st := range s.stores {
			out[i] = proto.DirEnt{Name: st.name, Attr: s.attr(st.root)}
		}
		return out, nil
	}
	if !n.dir {
		return nil, proto.ErrNotDir
	}
	dc, err := s.children(n)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]proto.DirEnt, 0, len(dc.nodes))
	for _, c := range dc.nodes {
		out = append(out, proto.DirEnt{Name: c.name, Attr: s.attrLocked(c)})
	}
	return out, nil
}

func (b *Backend) file(rel string) (*session, *node, error) {
	s, err := b.sess()
	if err != nil {
		return nil, nil, err
	}
	n, err := s.resolve(rel)
	if err != nil {
		return nil, nil, err
	}
	if n.dir {
		return nil, nil, proto.ErrIsDir
	}
	return s, n, nil
}

func (b *Backend) Read(rel string, off uint64, count uint32) ([]byte, bool, proto.Attr, error) {
	s, n, err := b.file(rel)
	if err != nil {
		return nil, false, proto.Attr{}, err
	}
	// Finish an edit nobody is writing to before reading back: some devices
	// only serve what has been committed.
	s.endEdit(n.handle, false)
	a := s.attr(n)
	if off >= a.Size {
		return nil, true, a, nil
	}
	if rest := a.Size - off; uint64(count) > rest {
		count = uint32(rest)
	}
	ctx, cancel := s.data()
	defer cancel()
	data, err := s.conn.ReadAt(ctx, n.handle, off, count)
	if err != nil {
		return nil, false, a, xlate(err)
	}
	return data, off+uint64(len(data)) >= a.Size, a, nil
}

func (b *Backend) Write(rel string, off uint64, data []byte) (pre, post proto.Attr, err error) {
	s, n, err := b.file(rel)
	if err != nil {
		return pre, post, err
	}
	if !s.writable {
		return pre, post, proto.ErrROFS
	}
	if err := s.acquireEdit(n.handle); err != nil {
		return pre, post, err
	}
	defer s.releaseEdit(n.handle)
	pre = s.attr(n)
	ctx, cancel := s.data()
	defer cancel()
	if err := s.conn.WriteAt(ctx, n.handle, off, data); err != nil {
		return pre, post, xlate(err)
	}
	s.noteWrite(n, off+uint64(len(data)), false)
	return pre, s.attr(n), nil
}

func (b *Backend) Truncate(rel string, size uint64) error {
	s, n, err := b.file(rel)
	if err != nil {
		return err
	}
	if !s.writable {
		return proto.ErrROFS
	}
	if err := s.acquireEdit(n.handle); err != nil {
		return err
	}
	defer s.releaseEdit(n.handle)
	ctx, cancel := s.meta()
	defer cancel()
	if err := s.conn.Truncate(ctx, n.handle, size); err != nil {
		return xlate(err)
	}
	s.noteWrite(n, size, true)
	return nil
}

// Fsync finishes the edit: that is the point at which the device commits
// the file and makes it visible to its media index.
func (b *Backend) Fsync(rel string) error {
	s, n, err := b.file(rel)
	if err != nil {
		return err
	}
	return s.endEdit(n.handle, false)
}

func (b *Backend) parentOf(rel string) (*session, *node, string, error) {
	s, err := b.sess()
	if err != nil {
		return nil, nil, "", err
	}
	if !s.writable {
		return nil, nil, "", proto.ErrROFS
	}
	dir, name := splitRel(rel)
	p, err := s.resolve(dir)
	if err != nil {
		return nil, nil, "", err
	}
	if p.virtual {
		return nil, nil, "", proto.ErrAcces // storages cannot be created or removed
	}
	if !p.dir {
		return nil, nil, "", proto.ErrNotDir
	}
	return s, p, name, nil
}

func (s *session) make(p *node, name string, dir bool) (*node, error) {
	ctx, cancel := s.meta()
	defer cancel()
	h, err := s.conn.Create(ctx, p.storage, p.handle, name, dir)
	if err != nil {
		return nil, xlate(err)
	}
	n := &node{storage: p.storage, handle: h, name: name, dir: dir, mtime: time.Now(), infoAt: time.Now()}
	s.adopt(p, n)
	return n, nil
}

func (b *Backend) Create(rel string, mode uint32, excl, trunc bool) (proto.Attr, error) {
	s, p, name, err := b.parentOf(rel)
	if err != nil {
		return proto.Attr{}, err
	}
	dc, err := s.children(p)
	if err != nil {
		return proto.Attr{}, err
	}
	s.mu.Lock()
	old := dc.byName[name]
	s.mu.Unlock()
	if old != nil {
		switch {
		case excl:
			return proto.Attr{}, proto.ErrExist
		case old.dir:
			return proto.Attr{}, proto.ErrIsDir
		}
		if trunc {
			if err := b.Truncate(rel, 0); err != nil {
				return proto.Attr{}, err
			}
		}
		return s.attr(old), nil
	}
	n, err := s.make(p, name, false)
	if err != nil {
		return proto.Attr{}, err
	}
	return s.attr(n), nil
}

func (b *Backend) Mkdir(rel string, mode uint32) (proto.Attr, error) {
	s, p, name, err := b.parentOf(rel)
	if err != nil {
		return proto.Attr{}, err
	}
	dc, err := s.children(p)
	if err != nil {
		return proto.Attr{}, err
	}
	s.mu.Lock()
	old := dc.byName[name]
	s.mu.Unlock()
	if old != nil {
		return proto.Attr{}, proto.ErrExist
	}
	n, err := s.make(p, name, true)
	if err != nil {
		return proto.Attr{}, err
	}
	return s.attr(n), nil
}

// lookupChild resolves name inside p.
func (s *session) lookupChild(p *node, name string) (*node, error) {
	dc, err := s.children(p)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	n := dc.byName[name]
	s.mu.Unlock()
	if n == nil {
		return nil, proto.ErrNoEnt
	}
	return n, nil
}

// empty asks the device (not the cache) whether a folder has children: MTP
// deletes folders recursively, so this check is what stands between rmdir
// and the loss of a whole tree.
func (s *session) empty(n *node) (bool, error) {
	ctx, cancel := s.meta()
	defer cancel()
	hs, err := s.conn.GetObjectHandles(ctx, n.storage, n.handle)
	if err != nil {
		return false, xlate(err)
	}
	return len(hs) == 0, nil
}

func (s *session) remove(p, n *node) error {
	if !n.dir {
		s.endEdit(n.handle, true)
	}
	ctx, cancel := s.meta()
	defer cancel()
	if err := s.conn.Delete(ctx, n.handle); err != nil {
		if isResp(err, mtp.RespInvalidObjectHandle) {
			s.forget(p, n)
		}
		return xlate(err)
	}
	s.forget(p, n)
	return nil
}

func (b *Backend) Remove(rel string) error {
	s, p, name, err := b.parentOf(rel)
	if err != nil {
		return err
	}
	n, err := s.lookupChild(p, name)
	if err != nil {
		return err
	}
	if n.dir {
		return proto.ErrIsDir
	}
	return s.remove(p, n)
}

func (b *Backend) Rmdir(rel string) error {
	s, p, name, err := b.parentOf(rel)
	if err != nil {
		return err
	}
	n, err := s.lookupChild(p, name)
	if err != nil {
		return err
	}
	if !n.dir {
		return proto.ErrNotDir
	}
	if ok, err := s.empty(n); err != nil {
		return err
	} else if !ok {
		return proto.ErrNotEmpty
	}
	return s.remove(p, n)
}

// Rename moves and/or renames an object. Replacing an existing target is
// done as delete-then-rename, which unlike POSIX rename is not atomic: MTP
// has nothing better.
func (b *Backend) Rename(from, to string) error {
	s, sp, sname, err := b.parentOf(from)
	if err != nil {
		return err
	}
	_, dp, dname, err := b.parentOf(to)
	if err != nil {
		return err
	}
	n, err := s.lookupChild(sp, sname)
	if err != nil {
		return err
	}
	if sp.storage != dp.storage {
		return proto.ErrXDev
	}
	moving := sp.storage != dp.storage || sp.handle != dp.handle
	if moving && !s.conn.Info.Supports(mtp.OpMoveObject) {
		return proto.ErrXDev // the caller falls back to copy + delete
	}
	if sname != dname && !s.conn.Info.Supports(mtp.OpSetObjectPropValue) {
		return proto.ErrNotSupp
	}
	if old, err := s.lookupChild(dp, dname); err == nil {
		switch {
		case old == n:
			return nil
		case old.dir && !n.dir:
			return proto.ErrIsDir
		case !old.dir && n.dir:
			return proto.ErrNotDir
		}
		if old.dir {
			if ok, err := s.empty(old); err != nil {
				return err
			} else if !ok {
				return proto.ErrNotEmpty
			}
		}
		if err := s.remove(dp, old); err != nil {
			return err
		}
	} else if err != proto.ErrNoEnt {
		return err
	}
	if !n.dir {
		s.endEdit(n.handle, true)
	}
	ctx, cancel := s.meta()
	defer cancel()
	if moving {
		parent := dp.handle
		if parent == mtp.RootParent {
			parent = 0 // MoveObject spells "top of the storage" differently
		}
		if err := s.conn.Move(ctx, n.handle, dp.storage, parent); err != nil {
			return xlate(err)
		}
	}
	if sname != dname {
		if err := s.conn.Rename(ctx, n.handle, dname); err != nil {
			if moving { // half done: at least keep the cache truthful
				s.mu.Lock()
				s.unlinkLocked(sp, n)
				s.mu.Unlock()
				s.adopt(dp, n)
			}
			return xlate(err)
		}
	}
	s.mu.Lock()
	s.unlinkLocked(sp, n)
	n.name = dname
	s.mu.Unlock()
	s.adopt(dp, n)
	return nil
}

// Utimes: MTP cannot set a modification time on Android.
func (b *Backend) Utimes(rel string, sec int64, nsec uint32) error { return proto.ErrNotSupp }

func (b *Backend) Statfs() (proto.Statfs, error) {
	s, err := b.sess()
	if err != nil {
		return proto.Statfs{}, err
	}
	ctx, cancel := s.meta()
	defer cancel()
	const bsize = 4096
	out := proto.Statfs{Bsize: bsize, Files: 1 << 20, Ffree: 1 << 20}
	for _, st := range s.stores {
		si, err := s.conn.GetStorageInfo(ctx, st.id)
		if err != nil {
			return proto.Statfs{}, xlate(err)
		}
		out.Blocks += si.Capacity / bsize
		out.Bfree += si.Free / bsize
		out.Bavail += si.Free / bsize
	}
	return out, nil
}

func (b *Backend) Symlink(target, rel string) (proto.Attr, error) {
	return proto.Attr{}, proto.ErrNotSupp
}

func (b *Backend) Readlink(rel string) (string, error) { return "", proto.ErrInval }
