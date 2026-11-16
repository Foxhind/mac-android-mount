package mtp

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	ErrOffline = errors.New("mtp: device is offline")
	ErrTimeout = errors.New("mtp: operation timed out")
	// ErrNoEvent is returned by Transport.ReadEvent when nothing arrived.
	ErrNoEvent = errors.New("mtp: no event")
)

// Transport moves raw packets. Every call returns within its timeout.
type Transport interface {
	// Write sends one bulk-out transfer (an empty slice is a zero-length
	// packet).
	Write(b []byte, timeout time.Duration) error
	// Read performs one bulk-in transfer: it returns when the buffer is
	// full or the device ends the transfer with a short packet.
	Read(b []byte, timeout time.Duration) (int, error)
	// ReadEvent reads from the interrupt pipe, or returns ErrNoEvent.
	ReadEvent(b []byte, timeout time.Duration) (int, error)
	// MaxPacket returns the bulk-in and bulk-out packet sizes.
	MaxPacket() (in, out int)
	// Close is only called when no other call is in progress.
	Close()
}

type Options struct {
	// SendTimeout bounds each outgoing transfer.
	SendTimeout time.Duration
	// ReplyTimeout bounds each wait for the device. It is deliberately
	// generous: phones go quiet for seconds while they index media. The
	// caller's own, shorter deadline is what keeps the Mac responsive; this
	// one only decides when a silent device is given up on.
	ReplyTimeout time.Duration
}

func DefaultOptions() Options {
	return Options{SendTimeout: 5 * time.Second, ReplyTimeout: 25 * time.Second}
}

type Event struct {
	Code   uint16
	Params []uint32
}

// Conn is an open MTP session. MTP allows one transaction at a time, so
// operations queue; a caller whose context expires stops waiting (and gets
// ErrTimeout) while the transaction itself runs to its own timeout.
//
// Any transport failure, including a timeout, leaves the protocol state
// unknown; the session is then dead for good and every call returns
// ErrOffline at once. Recovery is a new Conn.
type Conn struct {
	Info DeviceInfo

	t      Transport
	opt    Options
	inMax  int
	outMax int
	sem    chan struct{} // transaction lock
	tid    uint32
	pend   []byte // bytes read past the end of the previous container

	dead     chan struct{}
	deadOnce sync.Once
	err      error
	evDone   chan struct{}
}

// Open starts a session on t. onEvent (may be nil) is called from a
// separate goroutine for every event the device reports.
func Open(ctx context.Context, t Transport, opt Options, onEvent func(Event)) (*Conn, error) {
	c := &Conn{t: t, opt: opt, sem: make(chan struct{}, 1), dead: make(chan struct{}), evDone: make(chan struct{})}
	c.inMax, c.outMax = t.MaxPacket()
	if c.inMax <= 0 {
		c.inMax = 512
	}
	if c.outMax <= 0 {
		c.outMax = 512
	}
	_, err := run(c, ctx, func() (struct{}, error) {
		_, data, err := c.xact(OpGetDeviceInfo, nil, nil, false)
		if err != nil {
			return struct{}{}, err
		}
		if c.Info, err = parseDeviceInfo(data); err != nil {
			return struct{}{}, err
		}
		c.tid = 0
		_, _, err = c.xact(OpOpenSession, []uint32{1}, nil, false)
		if errors.Is(err, RespError(RespSessionAlreadyOpen)) {
			// Left over from a previous owner of the device: start clean.
			c.xact(OpCloseSession, nil, nil, false)
			c.tid = 0
			_, _, err = c.xact(OpOpenSession, []uint32{1}, nil, false)
		}
		return struct{}{}, err
	})
	if err != nil {
		c.kill(err)
		close(c.evDone)
		go c.shutdown(false)
		return nil, err
	}
	go c.eventLoop(onEvent)
	return c, nil
}

// Dead is closed when the session can no longer be used.
func (c *Conn) Dead() <-chan struct{} { return c.dead }

func (c *Conn) Err() error {
	select {
	case <-c.dead:
		return c.err
	default:
		return nil
	}
}

func (c *Conn) kill(err error) {
	c.deadOnce.Do(func() {
		c.err = err
		close(c.dead)
	})
}

// Close ends the session. It returns immediately; the transport is released
// in the background once any transfer still in progress has timed out.
func (c *Conn) Close() {
	alive := c.Err() == nil
	c.kill(errors.New("mtp: session closed"))
	go c.shutdown(alive)
}

func (c *Conn) shutdown(sayGoodbye bool) {
	<-c.evDone
	c.sem <- struct{}{} // wait for the transaction in flight, if any
	if sayGoodbye {
		c.xact(OpCloseSession, nil, nil, false) // lets the device commit open edits
	}
	c.t.Close()
}

func (c *Conn) eventLoop(onEvent func(Event)) {
	defer close(c.evDone)
	buf := make([]byte, 64)
	for {
		select {
		case <-c.dead:
			return
		default:
		}
		n, err := c.t.ReadEvent(buf, time.Second)
		if errors.Is(err, ErrNoEvent) {
			continue
		}
		if err != nil {
			// Typically the device was unplugged: this is how that is
			// noticed even when no file operation is running.
			c.kill(fmt.Errorf("mtp: event pipe: %w", err))
			return
		}
		if n < headerLen || onEvent == nil {
			continue
		}
		ev := Event{Code: binary.LittleEndian.Uint16(buf[6:])}
		for p := headerLen; p+4 <= n; p += 4 {
			ev.Params = append(ev.Params, binary.LittleEndian.Uint32(buf[p:]))
		}
		onEvent(ev)
	}
}

// run executes fn under the transaction lock, giving up waiting (not the
// transaction) when ctx expires or the session dies.
func run[T any](c *Conn, ctx context.Context, fn func() (T, error)) (T, error) {
	var zero T
	select {
	case <-c.dead:
		return zero, ErrOffline
	default:
	}
	select {
	case c.sem <- struct{}{}:
	case <-c.dead:
		return zero, ErrOffline
	case <-ctx.Done():
		return zero, ErrTimeout
	}
	type result struct {
		v   T
		err error
	}
	done := make(chan result, 1)
	go func() {
		var r result
		select {
		case <-c.dead:
			r.err = ErrOffline
		default:
			r.v, r.err = fn()
		}
		<-c.sem
		done <- r
	}()
	select {
	case r := <-done:
		return r.v, r.err
	case <-c.dead:
		return zero, ErrOffline
	case <-ctx.Done():
		return zero, ErrTimeout
	}
}

func header(length uint32, typ, code uint16, tid uint32) []byte {
	b := make([]byte, headerLen, headerLen+20)
	binary.LittleEndian.PutUint32(b[0:], length)
	binary.LittleEndian.PutUint16(b[4:], typ)
	binary.LittleEndian.PutUint16(b[6:], code)
	binary.LittleEndian.PutUint32(b[8:], tid)
	return b
}

// fatal marks the session dead because of a transport-level failure.
func (c *Conn) fatal(err error) error {
	c.kill(err)
	return ErrOffline
}

// xact performs one transaction. The caller holds the transaction lock.
func (c *Conn) xact(code uint16, params []uint32, out []byte, hasOut bool) (rparams []uint32, in []byte, err error) {
	tid := c.tid
	c.tid++
	cmd := header(uint32(headerLen+4*len(params)), containerCommand, code, tid)
	for _, p := range params {
		cmd = binary.LittleEndian.AppendUint32(cmd, p)
	}
	if err := c.t.Write(cmd, c.opt.SendTimeout); err != nil {
		return nil, nil, c.fatal(fmt.Errorf("mtp: send command 0x%04X: %w", code, err))
	}
	if hasOut {
		buf := make([]byte, 0, headerLen+len(out))
		buf = append(buf, header(uint32(headerLen+len(out)), containerData, code, tid)...)
		buf = append(buf, out...)
		if err := c.t.Write(buf, c.opt.ReplyTimeout); err != nil {
			return nil, nil, c.fatal(fmt.Errorf("mtp: send data for 0x%04X: %w", code, err))
		}
		if len(buf)%c.outMax == 0 {
			// A transfer that ends on a packet boundary is terminated by a
			// zero-length packet.
			if err := c.t.Write(nil, c.opt.SendTimeout); err != nil {
				return nil, nil, c.fatal(fmt.Errorf("mtp: send terminator: %w", err))
			}
		}
	}
	for {
		typ, rcode, rtid, payload, err := c.readContainer()
		if err != nil {
			return nil, nil, c.fatal(fmt.Errorf("mtp: reply to 0x%04X: %w", code, err))
		}
		switch typ {
		case containerData:
			in = payload
		case containerResponse:
			if rtid != tid && code != OpGetDeviceInfo && code != OpOpenSession {
				continue // answer to something abandoned earlier
			}
			for p := 0; p+4 <= len(payload); p += 4 {
				rparams = append(rparams, binary.LittleEndian.Uint32(payload[p:]))
			}
			if rcode != RespOK {
				return rparams, nil, RespError(rcode)
			}
			return rparams, in, nil
		default:
			return nil, nil, c.fatal(fmt.Errorf("mtp: unexpected container type %d", typ))
		}
	}
}

const readChunk = 64 << 10 // a multiple of every bulk packet size

func (c *Conn) readContainer() (typ, code uint16, tid uint32, payload []byte, err error) {
	var buf []byte
	if len(c.pend) >= headerLen {
		buf, c.pend = c.pend, nil
	} else {
		c.pend = nil
		tmp := make([]byte, readChunk)
		for tries := 0; ; tries++ {
			n, err := c.t.Read(tmp, c.opt.ReplyTimeout)
			if err != nil {
				return 0, 0, 0, nil, err
			}
			if n > 0 {
				buf = tmp[:n]
				break
			}
			if tries > 3 { // zero-length packets only terminate transfers
				return 0, 0, 0, nil, errMalformed
			}
		}
	}
	if len(buf) < headerLen {
		return 0, 0, 0, nil, errMalformed
	}
	length := binary.LittleEndian.Uint32(buf)
	typ = binary.LittleEndian.Uint16(buf[4:])
	code = binary.LittleEndian.Uint16(buf[6:])
	tid = binary.LittleEndian.Uint32(buf[8:])
	if length < headerLen || length > 64<<20 {
		return 0, 0, 0, nil, errMalformed
	}
	total := int(length)
	if len(buf) > total {
		// The device did not terminate the transfer where the container
		// ended; the rest is the beginning of the next container.
		c.pend = append([]byte(nil), buf[total:]...)
		buf = buf[:total]
	}
	payload = make([]byte, len(buf)-headerLen, total-headerLen)
	copy(payload, buf[headerLen:])
	for len(payload) < total-headerLen {
		// Ask for exactly what is missing so that this transfer cannot run
		// into the next container.
		want := min(total-headerLen-len(payload), readChunk)
		payload = payload[:len(payload)+want]
		n, err := c.t.Read(payload[len(payload)-want:], c.opt.ReplyTimeout)
		if err != nil {
			return 0, 0, 0, nil, err
		}
		if n == 0 {
			return 0, 0, 0, nil, errMalformed
		}
		payload = payload[:len(payload)-want+n]
	}
	return typ, code, tid, payload, nil
}

// ------------------------------------------------------------- operations

func (c *Conn) CanEdit() bool {
	return c.Info.Supports(OpBeginEditObject) && c.Info.Supports(OpSendPartialObject) &&
		c.Info.Supports(OpTruncateObject) && c.Info.Supports(OpEndEditObject)
}

func (c *Conn) GetStorageIDs(ctx context.Context) ([]uint32, error) {
	return run(c, ctx, func() ([]uint32, error) {
		_, data, err := c.xact(OpGetStorageIDs, nil, nil, false)
		if err != nil {
			return nil, err
		}
		d := dec{b: data}
		ids := d.u32s()
		return ids, d.err
	})
}

func (c *Conn) GetStorageInfo(ctx context.Context, id uint32) (StorageInfo, error) {
	return run(c, ctx, func() (StorageInfo, error) {
		_, data, err := c.xact(OpGetStorageInfo, []uint32{id}, nil, false)
		if err != nil {
			return StorageInfo{}, err
		}
		si, err := parseStorageInfo(data)
		si.ID = id
		return si, err
	})
}

// GetObjectHandles lists the children of parent (RootParent for the top).
func (c *Conn) GetObjectHandles(ctx context.Context, storage, parent uint32) ([]uint32, error) {
	return run(c, ctx, func() ([]uint32, error) {
		_, data, err := c.xact(OpGetObjectHandles, []uint32{storage, 0, parent}, nil, false)
		if err != nil {
			return nil, err
		}
		d := dec{b: data}
		hs := d.u32s()
		return hs, d.err
	})
}

func (c *Conn) GetObjectInfo(ctx context.Context, h uint32) (ObjectInfo, error) {
	return run(c, ctx, func() (ObjectInfo, error) {
		_, data, err := c.xact(OpGetObjectInfo, []uint32{h}, nil, false)
		if err != nil {
			return ObjectInfo{}, err
		}
		return parseObjectInfo(data)
	})
}

// GetObjectSize returns the 64-bit size of an object.
func (c *Conn) GetObjectSize(ctx context.Context, h uint32) (uint64, error) {
	return run(c, ctx, func() (uint64, error) {
		_, data, err := c.xact(OpGetObjectPropValue, []uint32{h, PropObjectSize}, nil, false)
		if err != nil {
			return 0, err
		}
		d := dec{b: data}
		v := d.u64()
		return v, d.err
	})
}

// ReadAt reads up to n bytes at off; fewer are returned at end of file.
func (c *Conn) ReadAt(ctx context.Context, h uint32, off uint64, n uint32) ([]byte, error) {
	return run(c, ctx, func() ([]byte, error) {
		var data []byte
		var err error
		switch {
		case c.Info.Supports(OpGetPartialObject64):
			_, data, err = c.xact(OpGetPartialObject64, []uint32{h, uint32(off), uint32(off >> 32), n}, nil, false)
		case off+uint64(n) <= 0xFFFFFFFF:
			_, data, err = c.xact(OpGetPartialObject, []uint32{h, uint32(off), n}, nil, false)
		default:
			return nil, RespError(RespOperationNotSupp)
		}
		return data, err
	})
}

func (c *Conn) simple(ctx context.Context, code uint16, params ...uint32) error {
	_, err := run(c, ctx, func() (struct{}, error) {
		_, _, err := c.xact(code, params, nil, false)
		return struct{}{}, err
	})
	return err
}

func (c *Conn) BeginEdit(ctx context.Context, h uint32) error {
	return c.simple(ctx, OpBeginEditObject, h)
}

func (c *Conn) EndEdit(ctx context.Context, h uint32) error {
	return c.simple(ctx, OpEndEditObject, h)
}

func (c *Conn) Truncate(ctx context.Context, h uint32, size uint64) error {
	return c.simple(ctx, OpTruncateObject, h, uint32(size), uint32(size>>32))
}

// WriteAt writes data at off into an object that is being edited.
func (c *Conn) WriteAt(ctx context.Context, h uint32, off uint64, data []byte) error {
	data = append([]byte(nil), data...) // the transaction may outlive the caller
	_, err := run(c, ctx, func() (struct{}, error) {
		_, _, err := c.xact(OpSendPartialObject, []uint32{h, uint32(off), uint32(off >> 32), uint32(len(data))}, data, true)
		return struct{}{}, err
	})
	return err
}

// Create makes an empty file or a folder and returns its handle.
func (c *Conn) Create(ctx context.Context, storage, parent uint32, name string, dir bool) (uint32, error) {
	return run(c, ctx, func() (uint32, error) {
		oi := ObjectInfo{Storage: storage, Parent: parent, Name: name, Format: FormatForName(name), Modified: time.Now()}
		if dir {
			oi.Format = FormatAssociation
		}
		rp, _, err := c.xact(OpSendObjectInfo, []uint32{storage, parent}, oi.encode(), true)
		if err != nil {
			return 0, err
		}
		if len(rp) < 3 {
			return 0, errMalformed
		}
		if !dir {
			// The object's (empty) content must follow immediately.
			if _, _, err := c.xact(OpSendObject, nil, nil, true); err != nil {
				return 0, err
			}
		}
		return rp[2], nil
	})
}

func (c *Conn) Delete(ctx context.Context, h uint32) error {
	return c.simple(ctx, OpDeleteObject, h, 0)
}

func (c *Conn) Move(ctx context.Context, h, storage, parent uint32) error {
	return c.simple(ctx, OpMoveObject, h, storage, parent)
}

func (c *Conn) Rename(ctx context.Context, h uint32, name string) error {
	var e enc
	e.str(name)
	_, err := run(c, ctx, func() (struct{}, error) {
		_, _, err := c.xact(OpSetObjectPropValue, []uint32{h, PropObjectFileName}, e.b, true)
		return struct{}{}, err
	})
	return err
}
