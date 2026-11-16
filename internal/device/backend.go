package device

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"droidfs/internal/proto"
)

// DialFunc opens a fresh agent stream (banner already verified).
type DialFunc func(ctx context.Context) (io.ReadWriteCloser, error)

// Options are the time bounds of the backend. Every operation is bounded by
// one of them, so a caller never waits longer than the largest value here.
type Options struct {
	LinkTimeout time.Duration // silence on the stream before it is dead
	MetaTimeout time.Duration // stat, create, rename, ...
	DataTimeout time.Duration // one read or write of up to 1 MiB
	SyncTimeout time.Duration // fsync
	ListTimeout time.Duration // directory listing (can be huge)
}

func DefaultOptions() Options {
	return Options{
		LinkTimeout: 6 * time.Second,
		MetaTimeout: 12 * time.Second,
		DataTimeout: 20 * time.Second,
		SyncTimeout: 25 * time.Second,
		ListTimeout: 120 * time.Second,
	}
}

type conn struct {
	meta, data *Session
	root       string
	down       chan struct{}
}

// Backend is a reconnectable handle to one device. While disconnected every
// operation returns ErrOffline immediately.
type Backend struct {
	dial    DialFunc
	rootReq string
	opt     Options

	mu    sync.RWMutex
	cur   *conn
	epoch atomic.Uint64
}

func NewBackend(dial DialFunc, root string, opt Options) *Backend {
	return &Backend{dial: dial, rootReq: root, opt: opt}
}

// Connect establishes the agent sessions. On success the backend is online
// until the channel returned by Down is closed.
func (b *Backend) Connect(ctx context.Context) error {
	b.Disconnect()
	var sess [2]*Session
	fail := func(err error) error {
		for _, s := range sess {
			if s != nil {
				s.Close()
			}
		}
		return err
	}
	for i := range sess {
		rwc, err := b.dial(ctx)
		if err != nil {
			return fail(err)
		}
		sess[i] = NewSession(rwc, b.opt.LinkTimeout)
	}
	hctx, cancel := context.WithTimeout(ctx, b.opt.MetaTimeout)
	defer cancel()
	var e proto.Enc
	e.Str(b.rootReq)
	out, err := sess[0].Call(hctx, proto.OpHello, e.B)
	if err != nil {
		return fail(fmt.Errorf("agent handshake for %q: %w", b.rootReq, err))
	}
	d := proto.Dec{B: out}
	ver, _, root := d.U32(), d.U32(), d.Str()
	if d.Err != nil || ver != proto.Version {
		return fail(fmt.Errorf("agent protocol mismatch (got %d, want %d)", ver, proto.Version))
	}
	c := &conn{meta: sess[0], data: sess[1], root: root, down: make(chan struct{})}
	b.mu.Lock()
	b.cur = c
	b.epoch.Add(1)
	b.mu.Unlock()
	go func() {
		select {
		case <-c.meta.Done():
		case <-c.data.Done():
		}
		b.drop(c)
	}()
	return nil
}

func (b *Backend) drop(c *conn) {
	b.mu.Lock()
	if b.cur == c {
		b.cur = nil
	}
	b.mu.Unlock()
	c.meta.Close()
	c.data.Close()
	select {
	case <-c.down:
	default:
		close(c.down)
	}
}

// Disconnect takes the backend offline.
func (b *Backend) Disconnect() {
	b.mu.RLock()
	c := b.cur
	b.mu.RUnlock()
	if c != nil {
		b.drop(c)
	}
}

// Down returns a channel that is closed when the current connection is
// lost. If the backend is offline it is already closed.
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

// Root is the resolved remote root of the current connection.
func (b *Backend) Root() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.cur == nil {
		return b.rootReq
	}
	return b.cur.root
}

// Epoch changes every time the device is (re)connected.
func (b *Backend) Epoch() uint64 { return b.epoch.Load() }

func (b *Backend) conn() (*conn, error) {
	b.mu.RLock()
	c := b.cur
	b.mu.RUnlock()
	if c == nil {
		return nil, ErrOffline
	}
	return c, nil
}

func (c *conn) abs(rel string) string {
	switch {
	case rel == "":
		return c.root
	case c.root == "/":
		return "/" + rel
	}
	return c.root + "/" + rel
}

func call(s *Session, timeout time.Duration, op byte, payload []byte) (proto.Dec, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := s.Call(ctx, op, payload)
	return proto.Dec{B: out}, err
}

func attrResult(d proto.Dec, err error) (proto.Attr, error) {
	if err != nil {
		return proto.Attr{}, err
	}
	a := d.Attr()
	return a, d.Err
}

func (b *Backend) Stat(rel string) (proto.Attr, error) {
	c, err := b.conn()
	if err != nil {
		return proto.Attr{}, err
	}
	var e proto.Enc
	e.Str(c.abs(rel))
	return attrResult(call(c.meta, b.opt.MetaTimeout, proto.OpStat, e.B))
}

func (b *Backend) ReadDir(rel string) ([]proto.DirEnt, error) {
	c, err := b.conn()
	if err != nil {
		return nil, err
	}
	var e proto.Enc
	e.Str(c.abs(rel))
	d, err := call(c.meta, b.opt.ListTimeout, proto.OpReadDir, e.B)
	if err != nil {
		return nil, err
	}
	n := d.U32()
	if d.Err != nil || int(n) > len(d.B)/proto.AttrLen {
		return nil, proto.ErrShort
	}
	ents := make([]proto.DirEnt, 0, n)
	for i := uint32(0); i < n; i++ {
		name := d.Str()
		attr := d.Attr()
		ents = append(ents, proto.DirEnt{Name: name, Attr: attr})
	}
	return ents, d.Err
}

func (b *Backend) Read(rel string, off uint64, n uint32) ([]byte, bool, proto.Attr, error) {
	c, err := b.conn()
	if err != nil {
		return nil, false, proto.Attr{}, err
	}
	var e proto.Enc
	e.Str(c.abs(rel))
	e.U64(off)
	e.U32(n)
	d, err := call(c.data, b.opt.DataTimeout, proto.OpRead, e.B)
	if err != nil {
		return nil, false, proto.Attr{}, err
	}
	eof := d.Bool()
	attr := d.Attr()
	data := d.Rest()
	return data, eof, attr, d.Err
}

// Write returns the file's attributes from just before and just after.
func (b *Backend) Write(rel string, off uint64, data []byte) (pre, post proto.Attr, err error) {
	c, err := b.conn()
	if err != nil {
		return pre, post, err
	}
	p := c.abs(rel)
	e := proto.Enc{B: make([]byte, 0, 12+len(p)+len(data))}
	e.Str(p)
	e.U64(off)
	e.Raw(data)
	d, err := call(c.data, b.opt.DataTimeout, proto.OpWrite, e.B)
	if err != nil {
		return pre, post, err
	}
	pre, post = d.Attr(), d.Attr()
	return pre, post, d.Err
}

func (b *Backend) Create(rel string, mode uint32, excl, trunc bool) (proto.Attr, error) {
	c, err := b.conn()
	if err != nil {
		return proto.Attr{}, err
	}
	var e proto.Enc
	e.Str(c.abs(rel))
	e.U32(mode)
	e.Bool(excl)
	e.Bool(trunc)
	return attrResult(call(c.meta, b.opt.MetaTimeout, proto.OpCreate, e.B))
}

func (b *Backend) Mkdir(rel string, mode uint32) (proto.Attr, error) {
	c, err := b.conn()
	if err != nil {
		return proto.Attr{}, err
	}
	var e proto.Enc
	e.Str(c.abs(rel))
	e.U32(mode)
	return attrResult(call(c.meta, b.opt.MetaTimeout, proto.OpMkdir, e.B))
}

func (b *Backend) simple(op byte, timeout time.Duration, rel string, extra func(*conn, *proto.Enc)) error {
	c, err := b.conn()
	if err != nil {
		return err
	}
	var e proto.Enc
	e.Str(c.abs(rel))
	if extra != nil {
		extra(c, &e)
	}
	_, err = call(c.meta, timeout, op, e.B)
	return err
}

func (b *Backend) Remove(rel string) error {
	return b.simple(proto.OpRemove, b.opt.MetaTimeout, rel, nil)
}

func (b *Backend) Rmdir(rel string) error {
	return b.simple(proto.OpRmdir, b.opt.MetaTimeout, rel, nil)
}

func (b *Backend) Rename(from, to string) error {
	return b.simple(proto.OpRename, b.opt.MetaTimeout, from, func(c *conn, e *proto.Enc) {
		e.Str(c.abs(to))
	})
}

func (b *Backend) Truncate(rel string, size uint64) error {
	return b.simple(proto.OpTruncate, b.opt.MetaTimeout, rel, func(_ *conn, e *proto.Enc) {
		e.U64(size)
	})
}

func (b *Backend) Utimes(rel string, sec int64, nsec uint32) error {
	return b.simple(proto.OpUtimes, b.opt.MetaTimeout, rel, func(_ *conn, e *proto.Enc) {
		e.U64(uint64(sec))
		e.U32(nsec)
	})
}

func (b *Backend) Fsync(rel string) error {
	return b.simple(proto.OpFsync, b.opt.SyncTimeout, rel, nil)
}

func (b *Backend) Statfs() (proto.Statfs, error) {
	c, err := b.conn()
	if err != nil {
		return proto.Statfs{}, err
	}
	var e proto.Enc
	e.Str(c.root)
	d, err := call(c.meta, b.opt.MetaTimeout, proto.OpStatfs, e.B)
	if err != nil {
		return proto.Statfs{}, err
	}
	s := proto.Statfs{Bsize: d.U64(), Blocks: d.U64(), Bfree: d.U64(), Bavail: d.U64(), Files: d.U64(), Ffree: d.U64()}
	return s, d.Err
}

func (b *Backend) Symlink(target, rel string) (proto.Attr, error) {
	c, err := b.conn()
	if err != nil {
		return proto.Attr{}, err
	}
	var e proto.Enc
	e.Str(c.abs(rel))
	e.Str(target)
	return attrResult(call(c.meta, b.opt.MetaTimeout, proto.OpSymlink, e.B))
}

func (b *Backend) Readlink(rel string) (string, error) {
	c, err := b.conn()
	if err != nil {
		return "", err
	}
	var e proto.Enc
	e.Str(c.abs(rel))
	d, err := call(c.meta, b.opt.MetaTimeout, proto.OpReadlink, e.B)
	if err != nil {
		return "", err
	}
	s := d.Str()
	return s, d.Err
}
