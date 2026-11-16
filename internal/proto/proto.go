// Package proto is the wire protocol between the host daemon and the agent
// that runs on the Android device. It is a multiplexed request/response
// protocol over a single byte stream (the agent's stdin/stdout, carried by
// `adb exec:`).
//
//	request:  [len u32][id u32][op u8][payload]
//	response: [len u32][id u32][status u32][payload]
//
// len counts everything after itself. All integers are big-endian. Every
// operation is path based and stateless from the host's point of view, so a
// broken connection can be replaced by a fresh agent without losing anything.
package proto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	// Banner is written by the agent as soon as it starts, so the host can
	// tell a running agent from a shell error message.
	Banner  = "DFSAGNT1"
	Version = 1

	MaxIO       = 1 << 20  // largest single read/write
	MaxRequest  = 2 << 20  // largest request frame the agent accepts
	MaxResponse = 96 << 20 // largest response frame the host accepts
)

const (
	OpHello byte = iota + 1
	OpPing
	OpStat
	OpReadDir
	OpRead
	OpWrite
	OpCreate
	OpMkdir
	OpRemove
	OpRmdir
	OpRename
	OpTruncate
	OpUtimes
	OpFsync
	OpStatfs
	OpSymlink
	OpReadlink
)

// Status is an operation result. Non-zero values deliberately match the
// NFSv3 status codes so they pass through the stack unchanged.
type Status uint32

const (
	OK             Status = 0
	ErrPerm        Status = 1
	ErrNoEnt       Status = 2
	ErrIO          Status = 5
	ErrAcces       Status = 13
	ErrExist       Status = 17
	ErrXDev        Status = 18
	ErrNotDir      Status = 20
	ErrIsDir       Status = 21
	ErrInval       Status = 22
	ErrFBig        Status = 27
	ErrNoSpc       Status = 28
	ErrROFS        Status = 30
	ErrNameTooLong Status = 63
	ErrNotEmpty    Status = 66
	ErrDQuot       Status = 69
	ErrStale       Status = 70
	ErrBadHandle   Status = 10001
	ErrNotSupp     Status = 10004
	ErrTooSmall    Status = 10005
	ErrServerFault Status = 10006
	ErrJukebox     Status = 10008
)

func (s Status) Error() string {
	switch s {
	case OK:
		return "ok"
	case ErrPerm:
		return "operation not permitted"
	case ErrNoEnt:
		return "no such file or directory"
	case ErrIO:
		return "i/o error"
	case ErrAcces:
		return "permission denied"
	case ErrExist:
		return "file exists"
	case ErrXDev:
		return "cross-device link"
	case ErrNotDir:
		return "not a directory"
	case ErrIsDir:
		return "is a directory"
	case ErrInval:
		return "invalid argument"
	case ErrFBig:
		return "file too large"
	case ErrNoSpc:
		return "no space left on device"
	case ErrROFS:
		return "read-only file system"
	case ErrNameTooLong:
		return "file name too long"
	case ErrNotEmpty:
		return "directory not empty"
	case ErrDQuot:
		return "quota exceeded"
	case ErrStale:
		return "stale file handle"
	case ErrNotSupp:
		return "operation not supported"
	}
	return fmt.Sprintf("status %d", uint32(s))
}

// Unix file type bits (identical on Linux and Darwin).
const (
	ModeFmt  = 0o170000
	ModeSock = 0o140000
	ModeLnk  = 0o120000
	ModeReg  = 0o100000
	ModeBlk  = 0o060000
	ModeDir  = 0o040000
	ModeChr  = 0o020000
	ModeFifo = 0o010000
)

// Attr is the subset of stat(2) the host needs.
type Attr struct {
	Mode      uint32 // type bits + permissions
	Nlink     uint32
	Size      uint64
	Used      uint64 // bytes actually allocated
	MtimeSec  int64
	MtimeNsec uint32
	Ino       uint64
}

// AttrLen is the encoded size of an Attr.
const AttrLen = 44

func (a Attr) IsDir() bool { return a.Mode&ModeFmt == ModeDir }
func (a Attr) IsReg() bool { return a.Mode&ModeFmt == ModeReg }
func (a Attr) IsLnk() bool { return a.Mode&ModeFmt == ModeLnk }

func (e *Enc) Attr(a Attr) {
	e.U32(a.Mode)
	e.U32(a.Nlink)
	e.U64(a.Size)
	e.U64(a.Used)
	e.U64(uint64(a.MtimeSec))
	e.U32(a.MtimeNsec)
	e.U64(a.Ino)
}

func (d *Dec) Attr() Attr {
	var a Attr
	a.Mode = d.U32()
	a.Nlink = d.U32()
	a.Size = d.U64()
	a.Used = d.U64()
	a.MtimeSec = int64(d.U64())
	a.MtimeNsec = d.U32()
	a.Ino = d.U64()
	return a
}

// DirEnt is one directory entry with its attributes.
type DirEnt struct {
	Name string
	Attr Attr
}

// Statfs describes the file system holding the exported root.
type Statfs struct {
	Bsize, Blocks, Bfree, Bavail, Files, Ffree uint64
}

// Hello is the agent's answer to OpHello.
type Hello struct {
	Version uint32
	UID     uint32
	Root    string // exported root with symlinks resolved
}

// ---------------------------------------------------------------- encoding

type Enc struct{ B []byte }

func (e *Enc) U8(v byte)    { e.B = append(e.B, v) }
func (e *Enc) U32(v uint32) { e.B = binary.BigEndian.AppendUint32(e.B, v) }
func (e *Enc) U64(v uint64) { e.B = binary.BigEndian.AppendUint64(e.B, v) }
func (e *Enc) Bool(v bool) {
	if v {
		e.U8(1)
	} else {
		e.U8(0)
	}
}
func (e *Enc) Str(s string) {
	e.U32(uint32(len(s)))
	e.B = append(e.B, s...)
}

// Raw appends bytes without a length prefix (must be the last field).
func (e *Enc) Raw(b []byte) { e.B = append(e.B, b...) }

var ErrShort = errors.New("proto: truncated message")

type Dec struct {
	B   []byte
	Err error
}

func (d *Dec) take(n int) []byte {
	if d.Err != nil || n < 0 || len(d.B) < n {
		d.Err = ErrShort
		return nil
	}
	b := d.B[:n]
	d.B = d.B[n:]
	return b
}

func (d *Dec) U8() byte {
	if b := d.take(1); b != nil {
		return b[0]
	}
	return 0
}
func (d *Dec) Bool() bool { return d.U8() != 0 }
func (d *Dec) U32() uint32 {
	if b := d.take(4); b != nil {
		return binary.BigEndian.Uint32(b)
	}
	return 0
}
func (d *Dec) U64() uint64 {
	if b := d.take(8); b != nil {
		return binary.BigEndian.Uint64(b)
	}
	return 0
}
func (d *Dec) Str() string {
	n := d.U32()
	if d.Err != nil || n > uint32(len(d.B)) {
		d.Err = ErrShort
		return ""
	}
	return string(d.take(int(n)))
}

// Rest returns everything not yet consumed.
func (d *Dec) Rest() []byte {
	if d.Err != nil {
		return nil
	}
	b := d.B
	d.B = nil
	return b
}

// ------------------------------------------------------------------ frames

const hdrLen = 12 // len + id + (op|status) rounded: requests use 9, responses 12

// AppendRequest builds a complete request frame.
func AppendRequest(dst []byte, id uint32, op byte, payload []byte) []byte {
	dst = binary.BigEndian.AppendUint32(dst, uint32(5+len(payload)))
	dst = binary.BigEndian.AppendUint32(dst, id)
	dst = append(dst, op)
	return append(dst, payload...)
}

func ReadRequest(r io.Reader) (id uint32, op byte, payload []byte, err error) {
	var h [9]byte
	if _, err = io.ReadFull(r, h[:]); err != nil {
		return
	}
	n := binary.BigEndian.Uint32(h[0:])
	if n < 5 || n > MaxRequest {
		err = fmt.Errorf("proto: bad request length %d", n)
		return
	}
	id = binary.BigEndian.Uint32(h[4:])
	op = h[8]
	payload = make([]byte, n-5)
	_, err = io.ReadFull(r, payload)
	return
}

// ResponseHeader returns the fixed header that precedes a response payload.
func ResponseHeader(id uint32, st Status, payloadLen int) [hdrLen]byte {
	var h [hdrLen]byte
	binary.BigEndian.PutUint32(h[0:], uint32(8+payloadLen))
	binary.BigEndian.PutUint32(h[4:], id)
	binary.BigEndian.PutUint32(h[8:], uint32(st))
	return h
}

func ReadResponse(r io.Reader) (id uint32, st Status, payload []byte, err error) {
	var h [hdrLen]byte
	if _, err = io.ReadFull(r, h[:]); err != nil {
		return
	}
	n := binary.BigEndian.Uint32(h[0:])
	if n < 8 || n > MaxResponse {
		err = fmt.Errorf("proto: bad response length %d", n)
		return
	}
	id = binary.BigEndian.Uint32(h[4:])
	st = Status(binary.BigEndian.Uint32(h[8:]))
	payload = make([]byte, n-8)
	_, err = io.ReadFull(r, payload)
	return
}
