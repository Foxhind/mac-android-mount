// Package mtpsim is a software MTP responder for tests: it speaks the wire
// protocol at the level of USB transfers and packets and stores objects in
// a local directory. It imitates the behaviours of Android's MTP stack that
// matter to a host: one transaction at a time, transfers that end on a
// packet boundary are terminated by a zero-length packet, deleting a folder
// deletes its contents, an object's reported size is frozen while it is
// being edited, and handles do not survive a re-plug.
package mtpsim

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"

	"droidfs/internal/mtp"
)

const packet = 512

var (
	ErrNoDevice = errors.New("sim: no device")
	ErrTimeout  = errors.New("sim: transfer timed out")
	ErrOverflow = errors.New("sim: device sent more than the host asked for")
)

type object struct {
	path   string // relative to the storage root
	parent uint32
	dir    bool
}

// Device is the simulated phone. Its storage is the directory Root.
type Device struct {
	Root string
	// NoZLP makes the device omit the zero-length packet after transfers
	// that end on a packet boundary, as some firmware does.
	NoZLP bool
	// Storages, if set, are sub-directories of Root exposed as separate
	// storages; otherwise Root itself is the only storage.
	Storages []string

	mu        sync.Mutex
	gen       int // bumped on every re-plug
	plugged   bool
	frozen    bool
	objects   map[uint32]*object
	storageOf map[uint32]uint32
	byPath    map[string]uint32
	next      uint32
	edits     map[uint32]*editState
	link      *link
	Requests  int // number of transactions served (for tests)
}

type editState struct {
	f        *os.File
	baseSize int64
}

// link is one USB attachment.
type link struct {
	dev    *Device
	out    chan []byte // host -> device transfers
	in     chan []byte // device -> host packets
	events chan []byte
	gone   chan struct{}
	once   sync.Once
}

// New returns a simulated device serving root, already plugged in.
func New(root string) *Device {
	d := &Device{Root: root}
	d.Replug()
	return d
}

// Replug attaches the device afresh: new handles, no session, no edits.
func (d *Device) Replug() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dropLinkLocked()
	d.gen++
	d.plugged, d.frozen = true, false
	d.objects = map[uint32]*object{}
	d.storageOf = map[uint32]uint32{}
	d.byPath = map[string]uint32{}
	d.next = uint32(d.gen) * 1000000
	d.edits = map[uint32]*editState{}
}

func (d *Device) dropLinkLocked() {
	for _, e := range d.edits {
		e.f.Close()
	}
	d.edits = map[uint32]*editState{}
	if d.link != nil {
		l := d.link
		l.once.Do(func() { close(l.gone) })
		d.link = nil
	}
}

// Unplug pulls the cable: every transfer fails at once.
func (d *Device) Unplug() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.plugged = false
	d.dropLinkLocked()
}

// Freeze leaves the device attached but silent.
func (d *Device) Freeze() {
	d.mu.Lock()
	d.frozen = true
	d.mu.Unlock()
}

// Open returns the transport of the attached device.
func (d *Device) Open() (mtp.Transport, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.plugged {
		return nil, ErrNoDevice
	}
	d.dropLinkLocked()
	l := &link{
		dev:    d,
		out:    make(chan []byte, 16),
		in:     make(chan []byte, 1<<16),
		events: make(chan []byte, 16),
		gone:   make(chan struct{}),
	}
	d.link = l
	go l.serve()
	return l, nil
}

// ---------------------------------------------------------- the transport

func (l *link) MaxPacket() (int, int) { return packet, packet }

func (l *link) Close() { l.once.Do(func() { close(l.gone) }) }

func (l *link) Write(b []byte, timeout time.Duration) error {
	select {
	case <-l.gone:
		return ErrNoDevice
	default:
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case l.out <- append([]byte(nil), b...):
		return nil
	case <-l.gone:
		return ErrNoDevice
	case <-t.C:
		return ErrTimeout
	}
}

// Read behaves like a bulk-in transfer: it collects packets until the
// buffer is full or a short packet arrives.
func (l *link) Read(b []byte, timeout time.Duration) (int, error) {
	t := time.NewTimer(timeout)
	defer t.Stop()
	n := 0
	for n < len(b) {
		select {
		case <-l.gone:
			return n, ErrNoDevice
		default:
		}
		select {
		case p := <-l.in:
			if len(p) > len(b)-n {
				return n, ErrOverflow
			}
			n += copy(b[n:], p)
			if len(p) < packet {
				return n, nil
			}
		case <-l.gone:
			return n, ErrNoDevice
		case <-t.C:
			return n, ErrTimeout
		}
	}
	return n, nil
}

func (l *link) ReadEvent(b []byte, timeout time.Duration) (int, error) {
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-l.gone:
		return 0, ErrNoDevice
	default:
	}
	select {
	case p := <-l.events:
		return copy(b, p), nil
	case <-l.gone:
		return 0, ErrNoDevice
	case <-t.C:
		return 0, mtp.ErrNoEvent
	}
}

// send queues one device-to-host transfer.
func (l *link) send(b []byte) {
	for len(b) >= packet {
		l.push(b[:packet])
		b = b[packet:]
	}
	if len(b) > 0 || !l.dev.NoZLP {
		l.push(b) // the short (possibly empty) packet that ends the transfer
	}
}

func (l *link) push(p []byte) {
	select {
	case l.in <- append([]byte(nil), p...):
	case <-l.gone:
	}
}

func (l *link) recv() ([]byte, bool) {
	select {
	case b := <-l.out:
		return b, true
	case <-l.gone:
		return nil, false
	}
}

func container(typ, code uint16, tid uint32, payload []byte) []byte {
	b := make([]byte, 12, 12+len(payload))
	binary.LittleEndian.PutUint32(b, uint32(12+len(payload)))
	binary.LittleEndian.PutUint16(b[4:], typ)
	binary.LittleEndian.PutUint16(b[6:], code)
	binary.LittleEndian.PutUint32(b[8:], tid)
	return append(b, payload...)
}

func (l *link) serve() {
	d := l.dev
	var pendingObject uint32 // set by SendObjectInfo, consumed by SendObject
	for {
		cmd, ok := l.recv()
		if !ok {
			return
		}
		if len(cmd) < 12 || binary.LittleEndian.Uint16(cmd[4:]) != 1 {
			continue
		}
		code := binary.LittleEndian.Uint16(cmd[6:])
		tid := binary.LittleEndian.Uint32(cmd[8:])
		var params []uint32
		for p := 12; p+4 <= len(cmd); p += 4 {
			params = append(params, binary.LittleEndian.Uint32(cmd[p:]))
		}
		for len(params) < 5 {
			params = append(params, 0)
		}

		var dataOut []byte
		switch code {
		case mtp.OpSendObjectInfo, mtp.OpSendObject, mtp.OpSendPartialObject, mtp.OpSetObjectPropValue:
			b, ok := l.recv()
			if !ok {
				return
			}
			if len(b) < 12 || binary.LittleEndian.Uint16(b[4:]) != 2 {
				continue
			}
			if len(b)%packet == 0 {
				// The host owes us a zero-length packet.
				z, ok := l.recv()
				if !ok {
					return
				}
				if len(z) != 0 {
					l.send(container(3, mtp.RespGeneralError, tid, nil))
					continue
				}
			}
			dataOut = b[12:]
		}

		d.mu.Lock()
		frozen := d.frozen
		d.Requests++
		d.mu.Unlock()
		if frozen {
			continue // swallow the request: the host is left waiting
		}

		resp, rparams, dataIn := d.execute(code, params, dataOut, &pendingObject)
		if dataIn != nil && resp == mtp.RespOK {
			l.send(container(2, code, tid, dataIn))
		}
		var rp []byte
		for _, p := range rparams {
			rp = binary.LittleEndian.AppendUint32(rp, p)
		}
		l.send(container(3, resp, tid, rp))
	}
}

// ------------------------------------------------------------- the device

const storageBase = 0x00010001

func (d *Device) storageRoot(id uint32) (string, bool) {
	if len(d.Storages) == 0 {
		return d.Root, id == storageBase
	}
	i := int(id-storageBase) / 0x10000
	if id < storageBase || (id-storageBase)%0x10000 != 0 || i >= len(d.Storages) {
		return "", false
	}
	return filepath.Join(d.Root, d.Storages[i]), true
}

// handleFor returns the handle of a path, assigning one if needed.
func (d *Device) handleFor(storage uint32, rel string, parent uint32, dir bool) uint32 {
	key := string(rune(storage)) + rel
	if h, ok := d.byPath[key]; ok {
		return h
	}
	d.next++
	d.objects[d.next] = &object{path: rel, parent: parent, dir: dir}
	d.byPath[key] = d.next
	d.storageOf[d.next] = storage
	return d.next
}

func mtpString(s string) []byte {
	if s == "" {
		return []byte{0}
	}
	u := utf16.Encode([]rune(s))
	b := []byte{byte(len(u) + 1)}
	for _, c := range u {
		b = binary.LittleEndian.AppendUint16(b, c)
	}
	return append(b, 0, 0)
}

func readString(b []byte) (string, []byte) {
	if len(b) == 0 {
		return "", nil
	}
	n := int(b[0])
	b = b[1:]
	if n == 0 || len(b) < n*2 {
		return "", b
	}
	u := make([]uint16, 0, n)
	for i := 0; i < n; i++ {
		if c := binary.LittleEndian.Uint16(b[i*2:]); c != 0 {
			u = append(u, c)
		}
	}
	return string(utf16.Decode(u)), b[n*2:]
}

func u16array(v ...uint16) []byte {
	b := binary.LittleEndian.AppendUint32(nil, uint32(len(v)))
	for _, x := range v {
		b = binary.LittleEndian.AppendUint16(b, x)
	}
	return b
}

func u32array(v []uint32) []byte {
	b := binary.LittleEndian.AppendUint32(nil, uint32(len(v)))
	for _, x := range v {
		b = binary.LittleEndian.AppendUint32(b, x)
	}
	return b
}

func (d *Device) abs(h uint32) (string, *object, bool) {
	o := d.objects[h]
	if o == nil {
		return "", nil, false
	}
	root, ok := d.storageRoot(d.storageOf[h])
	if !ok {
		return "", nil, false
	}
	return filepath.Join(root, o.path), o, true
}

func (d *Device) execute(code uint16, p []uint32, data []byte, pending *uint32) (resp uint16, rparams []uint32, out []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	le := binary.LittleEndian
	switch code {
	case mtp.OpGetDeviceInfo:
		var b []byte
		b = le.AppendUint16(b, 100)
		b = le.AppendUint32(b, 6)
		b = le.AppendUint16(b, 100)
		b = append(b, mtpString("microsoft.com: 1.0; android.com: 1.0;")...)
		b = le.AppendUint16(b, 0)
		b = append(b, u16array(
			mtp.OpGetDeviceInfo, mtp.OpOpenSession, mtp.OpCloseSession, mtp.OpGetStorageIDs,
			mtp.OpGetStorageInfo, mtp.OpGetObjectHandles, mtp.OpGetObjectInfo, mtp.OpDeleteObject,
			mtp.OpSendObjectInfo, mtp.OpSendObject, mtp.OpMoveObject, mtp.OpGetPartialObject,
			mtp.OpGetObjectPropValue, mtp.OpSetObjectPropValue, mtp.OpGetPartialObject64,
			mtp.OpSendPartialObject, mtp.OpTruncateObject, mtp.OpBeginEditObject, mtp.OpEndEditObject)...)
		b = append(b, u16array(mtp.EvObjectAdded, mtp.EvObjectRemoved)...)
		b = append(b, u16array()...)
		b = append(b, u16array()...)
		b = append(b, u16array(mtp.FormatUndefined, mtp.FormatAssociation)...)
		b = append(b, mtpString("Sim")...)
		b = append(b, mtpString("Sim Phone")...)
		b = append(b, mtpString("1.0")...)
		b = append(b, mtpString("SIM0001")...)
		return mtp.RespOK, nil, b

	case mtp.OpOpenSession:
		return mtp.RespOK, nil, nil

	case mtp.OpCloseSession:
		for h, e := range d.edits {
			e.f.Close()
			delete(d.edits, h)
		}
		return mtp.RespOK, nil, nil

	case mtp.OpGetStorageIDs:
		n := max(len(d.Storages), 1)
		ids := make([]uint32, n)
		for i := range ids {
			ids[i] = storageBase + uint32(i)*0x10000
		}
		return mtp.RespOK, nil, u32array(ids)

	case mtp.OpGetStorageInfo:
		root, ok := d.storageRoot(p[0])
		if !ok {
			return mtp.RespInvalidStorageID, nil, nil
		}
		var st syscall.Statfs_t
		syscall.Statfs(root, &st)
		name := "Internal shared storage"
		if len(d.Storages) > 0 {
			name = d.Storages[int(p[0]-storageBase)/0x10000]
		}
		var b []byte
		b = le.AppendUint16(b, 3)
		b = le.AppendUint16(b, 2)
		b = le.AppendUint16(b, 0)
		b = le.AppendUint64(b, st.Blocks*uint64(st.Bsize))
		b = le.AppendUint64(b, st.Bavail*uint64(st.Bsize))
		b = le.AppendUint32(b, 0xFFFFFFFF)
		b = append(b, mtpString(name)...)
		b = append(b, mtpString("")...)
		return mtp.RespOK, nil, b

	case mtp.OpGetObjectHandles:
		root, ok := d.storageRoot(p[0])
		if !ok {
			return mtp.RespInvalidStorageID, nil, nil
		}
		rel, parent := "", uint32(0)
		if p[2] != mtp.RootParent && p[2] != 0 {
			o := d.objects[p[2]]
			if o == nil || !o.dir || d.storageOf[p[2]] != p[0] {
				return mtp.RespInvalidParentObject, nil, nil
			}
			rel, parent = o.path, p[2]
		}
		ents, err := os.ReadDir(filepath.Join(root, rel))
		if err != nil {
			return mtp.RespInvalidParentObject, nil, nil
		}
		hs := make([]uint32, 0, len(ents))
		for _, e := range ents {
			hs = append(hs, d.handleFor(p[0], filepath.Join(rel, e.Name()), parent, e.IsDir()))
		}
		return mtp.RespOK, nil, u32array(hs)

	case mtp.OpGetObjectInfo:
		path, o, ok := d.abs(p[0])
		if !ok {
			return mtp.RespInvalidObjectHandle, nil, nil
		}
		fi, err := os.Stat(path)
		if err != nil {
			return mtp.RespInvalidObjectHandle, nil, nil
		}
		size := fi.Size()
		if e := d.edits[p[0]]; e != nil {
			size = e.baseSize // like a media index that is only updated afterwards
		}
		format, s32 := uint16(mtp.FormatUndefined), uint32(0xFFFFFFFF)
		if size < 0xFFFFFFFF {
			s32 = uint32(size)
		}
		if o.dir {
			format, s32 = mtp.FormatAssociation, 0
		}
		var b []byte
		b = le.AppendUint32(b, d.storageOf[p[0]])
		b = le.AppendUint16(b, format)
		b = le.AppendUint16(b, 0)
		b = le.AppendUint32(b, s32)
		b = le.AppendUint16(b, 0)
		b = append(b, make([]byte, 24)...) // thumb + image fields
		b = le.AppendUint32(b, o.parent)
		b = le.AppendUint16(b, 0)
		b = le.AppendUint32(b, 0)
		b = le.AppendUint32(b, 0)
		b = append(b, mtpString(filepath.Base(o.path))...)
		b = append(b, mtpString("")...)
		b = append(b, mtpString(fi.ModTime().Local().Format("20060102T150405"))...)
		b = append(b, mtpString("")...)
		return mtp.RespOK, nil, b

	case mtp.OpGetObjectPropValue:
		path, _, ok := d.abs(p[0])
		if !ok {
			return mtp.RespInvalidObjectHandle, nil, nil
		}
		if p[1] != mtp.PropObjectSize {
			return mtp.RespOperationNotSupp, nil, nil
		}
		fi, err := os.Stat(path)
		if err != nil {
			return mtp.RespInvalidObjectHandle, nil, nil
		}
		return mtp.RespOK, nil, le.AppendUint64(nil, uint64(fi.Size()))

	case mtp.OpGetPartialObject, mtp.OpGetPartialObject64:
		path, o, ok := d.abs(p[0])
		if !ok || o.dir {
			return mtp.RespInvalidObjectHandle, nil, nil
		}
		off, n := int64(p[1]), p[2]
		if code == mtp.OpGetPartialObject64 {
			off, n = int64(p[1])|int64(p[2])<<32, p[3]
		}
		f, err := os.Open(path)
		if err != nil {
			return mtp.RespInvalidObjectHandle, nil, nil
		}
		defer f.Close()
		buf := make([]byte, n)
		got, _ := f.ReadAt(buf, off)
		return mtp.RespOK, []uint32{uint32(got)}, buf[:got]

	case mtp.OpSendObjectInfo:
		root, ok := d.storageRoot(p[0])
		if !ok {
			return mtp.RespInvalidStorageID, nil, nil
		}
		rel, parent := "", uint32(0)
		if p[1] != mtp.RootParent && p[1] != 0 {
			o := d.objects[p[1]]
			if o == nil || !o.dir {
				return mtp.RespInvalidParentObject, nil, nil
			}
			rel, parent = o.path, p[1]
		}
		if len(data) < 52 {
			return mtp.RespInvalidParameter, nil, nil
		}
		format := le.Uint16(data[4:])
		name, _ := readString(data[52:])
		if name == "" {
			return mtp.RespInvalidParameter, nil, nil
		}
		path := filepath.Join(root, rel, name)
		if _, err := os.Lstat(path); err == nil {
			return mtp.RespGeneralError, nil, nil
		}
		isDir := format == mtp.FormatAssociation
		var err error
		if isDir {
			err = os.Mkdir(path, 0o755)
		} else {
			err = os.WriteFile(path, nil, 0o644)
		}
		if err != nil {
			return mtp.RespGeneralError, nil, nil
		}
		h := d.handleFor(p[0], filepath.Join(rel, name), parent, isDir)
		if !isDir {
			*pending = h
		}
		return mtp.RespOK, []uint32{p[0], p[1], h}, nil

	case mtp.OpSendObject:
		path, _, ok := d.abs(*pending)
		*pending = 0
		if !ok {
			return mtp.RespGeneralError, nil, nil
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return mtp.RespGeneralError, nil, nil
		}
		return mtp.RespOK, nil, nil

	case mtp.OpDeleteObject:
		path, o, ok := d.abs(p[0])
		if !ok {
			return mtp.RespInvalidObjectHandle, nil, nil
		}
		if e := d.edits[p[0]]; e != nil {
			e.f.Close()
			delete(d.edits, p[0])
		}
		if err := os.RemoveAll(path); err != nil { // folders go with everything in them
			return mtp.RespGeneralError, nil, nil
		}
		st := d.storageOf[p[0]]
		for h, c := range d.objects {
			if d.storageOf[h] == st && (c.path == o.path || under(c.path, o.path)) {
				delete(d.byPath, string(rune(st))+c.path)
				delete(d.objects, h)
				delete(d.storageOf, h)
			}
		}
		return mtp.RespOK, nil, nil

	case mtp.OpMoveObject, mtp.OpSetObjectPropValue:
		path, o, ok := d.abs(p[0])
		if !ok {
			return mtp.RespInvalidObjectHandle, nil, nil
		}
		st := d.storageOf[p[0]]
		root, _ := d.storageRoot(st)
		var newRel string
		newParent := o.parent
		if code == mtp.OpMoveObject {
			if p[1] != st {
				return mtp.RespInvalidStorageID, nil, nil
			}
			dir := ""
			newParent = 0
			if p[2] != 0 {
				po := d.objects[p[2]]
				if po == nil || !po.dir {
					return mtp.RespInvalidParentObject, nil, nil
				}
				dir, newParent = po.path, p[2]
			}
			newRel = filepath.Join(dir, filepath.Base(o.path))
		} else {
			if p[1] != mtp.PropObjectFileName {
				return mtp.RespOperationNotSupp, nil, nil
			}
			name, _ := readString(data)
			if name == "" {
				return mtp.RespInvalidObjectPropVal, nil, nil
			}
			newRel = filepath.Join(filepath.Dir(o.path), name)
		}
		if _, err := os.Lstat(filepath.Join(root, newRel)); err == nil {
			return mtp.RespGeneralError, nil, nil // never overwrites
		}
		if err := os.Rename(path, filepath.Join(root, newRel)); err != nil {
			return mtp.RespGeneralError, nil, nil
		}
		old := o.path
		for h, c := range d.objects {
			if d.storageOf[h] != st {
				continue
			}
			if c.path == old || under(c.path, old) {
				delete(d.byPath, string(rune(st))+c.path)
				c.path = newRel + c.path[len(old):]
				d.byPath[string(rune(st))+c.path] = h
			}
		}
		o.parent = newParent
		return mtp.RespOK, nil, nil

	case mtp.OpBeginEditObject:
		path, o, ok := d.abs(p[0])
		if !ok || o.dir {
			return mtp.RespInvalidObjectHandle, nil, nil
		}
		if d.edits[p[0]] != nil {
			return mtp.RespGeneralError, nil, nil
		}
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			return mtp.RespGeneralError, nil, nil
		}
		fi, _ := f.Stat()
		d.edits[p[0]] = &editState{f: f, baseSize: fi.Size()}
		return mtp.RespOK, nil, nil

	case mtp.OpSendPartialObject:
		e := d.edits[p[0]]
		if e == nil {
			return mtp.RespGeneralError, nil, nil
		}
		if uint32(len(data)) != p[3] {
			return mtp.RespInvalidParameter, nil, nil
		}
		if _, err := e.f.WriteAt(data, int64(p[1])|int64(p[2])<<32); err != nil {
			return mtp.RespStoreFull, nil, nil
		}
		return mtp.RespOK, []uint32{uint32(len(data))}, nil

	case mtp.OpTruncateObject:
		e := d.edits[p[0]]
		if e == nil {
			return mtp.RespGeneralError, nil, nil
		}
		if err := e.f.Truncate(int64(p[1]) | int64(p[2])<<32); err != nil {
			return mtp.RespGeneralError, nil, nil
		}
		return mtp.RespOK, nil, nil

	case mtp.OpEndEditObject:
		e := d.edits[p[0]]
		if e == nil {
			return mtp.RespGeneralError, nil, nil
		}
		e.f.Close()
		delete(d.edits, p[0])
		return mtp.RespOK, nil, nil
	}
	return mtp.RespOperationNotSupp, nil, nil
}

func under(p, dir string) bool {
	return len(p) > len(dir) && p[len(dir)] == filepath.Separator && p[:len(dir)] == dir
}
