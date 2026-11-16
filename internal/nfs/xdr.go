package nfs

import (
	"encoding/binary"
	"errors"
)

var errXDR = errors.New("xdr: truncated or malformed")

type reader struct {
	b   []byte
	err error
}

func (r *reader) take(n int) []byte {
	if r.err != nil || n < 0 || n > len(r.b) {
		r.err = errXDR
		return nil
	}
	b := r.b[:n]
	r.b = r.b[n:]
	return b
}

func (r *reader) u32() uint32 {
	if b := r.take(4); b != nil {
		return binary.BigEndian.Uint32(b)
	}
	return 0
}

func (r *reader) u64() uint64 {
	if b := r.take(8); b != nil {
		return binary.BigEndian.Uint64(b)
	}
	return 0
}

// fixed reads n bytes plus padding to a 4-byte boundary.
func (r *reader) fixed(n int) []byte {
	b := r.take(n)
	if pad := (4 - n%4) % 4; pad != 0 {
		r.take(pad)
	}
	return b
}

// opaque reads a variable-length byte string of at most max bytes.
func (r *reader) opaque(max int) []byte {
	n := r.u32()
	if r.err != nil || int64(n) > int64(max) {
		r.err = errXDR
		return nil
	}
	return r.fixed(int(n))
}

func (r *reader) str(max int) string { return string(r.opaque(max)) }

type writer struct{ b []byte }

func (w *writer) u32(v uint32) { w.b = binary.BigEndian.AppendUint32(w.b, v) }
func (w *writer) u64(v uint64) { w.b = binary.BigEndian.AppendUint64(w.b, v) }
func (w *writer) boolean(v bool) {
	if v {
		w.u32(1)
	} else {
		w.u32(0)
	}
}

var zeroPad [4]byte

func (w *writer) fixed(b []byte) {
	w.b = append(w.b, b...)
	if pad := (4 - len(b)%4) % 4; pad != 0 {
		w.b = append(w.b, zeroPad[:pad]...)
	}
}

func (w *writer) opaque(b []byte) {
	w.u32(uint32(len(b)))
	w.fixed(b)
}

func (w *writer) str(s string) {
	w.u32(uint32(len(s)))
	w.b = append(w.b, s...)
	if pad := (4 - len(s)%4) % 4; pad != 0 {
		w.b = append(w.b, zeroPad[:pad]...)
	}
}

func padded(n int) int { return (n + 3) &^ 3 }
