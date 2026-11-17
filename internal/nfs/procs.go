package nfs

import (
	"encoding/binary"
	"time"

	"github.com/Foxhind/macdroidfs/internal/proto"
	"github.com/Foxhind/macdroidfs/internal/vfs"
)

const (
	fsid = 0x44524F4944 // "DROID"

	maxIO  = proto.MaxIO
	prefIO = 256 << 10

	access3Modify = 0x04
	access3Extend = 0x08
	access3Delete = 0x10
)

var procNames = [...]string{
	"NULL", "GETATTR", "SETATTR", "LOOKUP", "ACCESS", "READLINK", "READ",
	"WRITE", "CREATE", "MKDIR", "SYMLINK", "MKNOD", "REMOVE", "RMDIR",
	"RENAME", "LINK", "READDIR", "READDIRPLUS", "FSSTAT", "FSINFO",
	"PATHCONF", "COMMIT",
}

// ------------------------------------------------------------ wire helpers

func (s *Server) putFH(w *writer, id uint64) {
	w.u32(16)
	w.u64(s.fs.Instance())
	w.u64(id)
}

func (s *Server) fh(r *reader) (uint64, proto.Status) {
	b := r.opaque(64)
	if r.err != nil || len(b) != 16 {
		return 0, proto.ErrBadHandle
	}
	if binary.BigEndian.Uint64(b) != s.fs.Instance() {
		return 0, proto.ErrStale // handle from a previous run of the daemon
	}
	return binary.BigEndian.Uint64(b[8:]), proto.OK
}

func ftype(mode uint32) uint32 {
	switch mode & proto.ModeFmt {
	case proto.ModeDir:
		return 2
	case proto.ModeBlk:
		return 3
	case proto.ModeChr:
		return 4
	case proto.ModeLnk:
		return 5
	case proto.ModeSock:
		return 6
	case proto.ModeFifo:
		return 7
	}
	return 1
}

func (w *writer) nfstime(sec int64, nsec uint32) {
	if sec < 0 {
		sec, nsec = 0, 0
	} else if sec > 0xffffffff {
		sec = 0xffffffff
	}
	w.u32(uint32(sec))
	w.u32(nsec)
}

func (s *Server) fattr(w *writer, a *vfs.Attr) {
	w.u32(ftype(a.Mode))
	w.u32(a.Mode & 0o7777)
	w.u32(a.Nlink)
	w.u32(s.uid)
	w.u32(s.gid)
	w.u64(a.Size)
	w.u64(a.Used)
	w.u32(0) // rdev
	w.u32(0)
	w.u64(fsid)
	w.u64(a.FileID)
	w.nfstime(a.MtimeSec, a.MtimeNsec) // atime
	w.nfstime(a.MtimeSec, a.MtimeNsec) // mtime
	w.nfstime(a.MtimeSec, a.MtimeNsec) // ctime
}

// postOp writes a post_op_attr; nil means "not available".
func (s *Server) postOp(w *writer, a *vfs.Attr) {
	if a == nil {
		w.u32(0)
		return
	}
	w.u32(1)
	s.fattr(w, a)
}

// postOpCached writes whatever is known about id without asking the device.
func (s *Server) postOpCached(w *writer, id uint64) {
	if a, ok := s.fs.CachedAttr(id); ok {
		s.postOp(w, &a)
	} else {
		w.u32(0)
	}
}

func (s *Server) wcc(w *writer, pre, post *vfs.Attr) {
	if pre == nil {
		w.u32(0)
	} else {
		w.u32(1)
		w.u64(pre.Size)
		w.nfstime(pre.MtimeSec, pre.MtimeNsec) // mtime
		w.nfstime(pre.MtimeSec, pre.MtimeNsec) // ctime
	}
	s.postOp(w, post)
}

type sattr struct {
	mode  *uint32
	size  *uint64
	mtime *time.Time
}

func (r *reader) sattr() sattr {
	var s sattr
	if r.u32() != 0 {
		m := r.u32()
		s.mode = &m
	}
	if r.u32() != 0 {
		r.u32() // uid: ignored
	}
	if r.u32() != 0 {
		r.u32() // gid: ignored
	}
	if r.u32() != 0 {
		v := r.u64()
		s.size = &v
	}
	if r.u32() == 2 { // atime: ignored
		r.u32()
		r.u32()
	}
	if r.u32() == 2 { // mtime set to a client-supplied value
		sec, nsec := r.u32(), r.u32()
		t := time.Unix(int64(sec), int64(nsec))
		s.mtime = &t
	}
	return s
}

// failBody writes the part of a failure reply that follows the status, with
// all optional attributes absent.
func failBody(proc uint32, w *writer) {
	switch proc {
	case 1: // GETATTR: nothing
	case 2, 7, 8, 9, 10, 11, 12, 13, 21: // wcc_data
		w.u32(0)
		w.u32(0)
	case 14: // RENAME: two wcc_data
		w.u32(0)
		w.u32(0)
		w.u32(0)
		w.u32(0)
	case 15: // LINK: post_op_attr + wcc_data
		w.u32(0)
		w.u32(0)
		w.u32(0)
	default: // post_op_attr
		w.u32(0)
	}
}

// --------------------------------------------------------------- procedures

func (s *Server) nfsProc(proc uint32, r *reader, w *writer) uint32 {
	if proc >= uint32(len(procNames)) {
		return acceptProcUnavail
	}
	start := time.Now()
	st, ok := s.nfsDo(proc, r, w)
	if !ok {
		return acceptGarbageArgs
	}
	if s.Trace != nil {
		s.Trace("nfs %-11s -> %d (%s)", procNames[proc], uint32(st), time.Since(start).Round(time.Microsecond))
	}
	return acceptSuccess
}

// nfsDo runs one procedure. It returns false if the arguments could not be
// decoded; otherwise the reply body has been written to w.
func (s *Server) nfsDo(proc uint32, r *reader, w *writer) (st proto.Status, ok bool) {
	fs := s.fs
	// fail writes a failure reply.
	fail := func(st proto.Status) (proto.Status, bool) {
		w.u32(uint32(st))
		failBody(proc, w)
		return st, true
	}

	switch proc {
	case 0: // NULL
		return proto.OK, true

	case 1: // GETATTR
		id, hs := s.fh(r)
		if r.err != nil {
			return 0, false
		}
		if hs != proto.OK {
			return fail(hs)
		}
		a, st := fs.GetAttr(id)
		if st != proto.OK {
			return fail(st)
		}
		w.u32(0)
		s.fattr(w, &a)

	case 2: // SETATTR
		id, hs := s.fh(r)
		sa := r.sattr()
		if r.u32() != 0 { // guard: ignored
			r.u32()
			r.u32()
		}
		if r.err != nil {
			return 0, false
		}
		if hs != proto.OK {
			return fail(hs)
		}
		a, st := fs.SetAttr(id, sa.size, sa.mtime)
		if st != proto.OK {
			return fail(st)
		}
		w.u32(0)
		s.wcc(w, nil, &a)

	case 3: // LOOKUP
		dir, hs := s.fh(r)
		name := r.str(4096)
		if r.err != nil {
			return 0, false
		}
		if hs != proto.OK {
			return fail(hs)
		}
		a, st := fs.Lookup(dir, name)
		if st != proto.OK {
			w.u32(uint32(st))
			s.postOpCached(w, dir)
			return st, true
		}
		w.u32(0)
		s.putFH(w, a.FileID)
		s.postOp(w, &a)
		s.postOpCached(w, dir)

	case 4: // ACCESS
		id, hs := s.fh(r)
		mask := r.u32()
		if r.err != nil {
			return 0, false
		}
		if hs != proto.OK {
			return fail(hs)
		}
		a, st := fs.GetAttr(id)
		if st != proto.OK {
			return fail(st)
		}
		// The device decides for real when the operation is attempted.
		mask &= 0x3f
		if fs.ReadOnly() {
			mask &^= access3Modify | access3Extend | access3Delete
		}
		w.u32(0)
		s.postOp(w, &a)
		w.u32(mask)

	case 5: // READLINK
		id, hs := s.fh(r)
		if r.err != nil {
			return 0, false
		}
		if hs != proto.OK {
			return fail(hs)
		}
		target, st := fs.Readlink(id)
		if st != proto.OK {
			return fail(st)
		}
		w.u32(0)
		s.postOpCached(w, id)
		w.str(target)

	case 6: // READ
		id, hs := s.fh(r)
		off, count := r.u64(), r.u32()
		if r.err != nil {
			return 0, false
		}
		if hs != proto.OK {
			return fail(hs)
		}
		data, eof, st := fs.Read(id, off, min(count, maxIO))
		if st != proto.OK {
			return fail(st)
		}
		w.u32(0)
		s.postOpCached(w, id)
		w.u32(uint32(len(data)))
		w.boolean(eof)
		w.opaque(data)

	case 7: // WRITE
		id, hs := s.fh(r)
		off, count, stable := r.u64(), r.u32(), r.u32()
		data := r.opaque(maxRecord)
		if r.err != nil {
			return 0, false
		}
		if hs != proto.OK {
			return fail(hs)
		}
		if uint32(len(data)) > count {
			data = data[:count]
		}
		if len(data) > maxIO {
			data = data[:maxIO] // a short write; the client sends the rest
		}
		pre, post, st := fs.Write(id, off, data)
		if st == proto.OK && stable != 0 {
			st = fs.Commit(id)
		}
		if st != proto.OK {
			return fail(st)
		}
		w.u32(0)
		s.wcc(w, &pre, &post)
		w.u32(uint32(len(data)))
		if stable != 0 {
			w.u32(2) // FILE_SYNC
		} else {
			w.u32(0) // UNSTABLE
		}
		w.u64(fs.WriteVerf())

	case 8: // CREATE
		dir, hs := s.fh(r)
		name := r.str(4096)
		how := r.u32()
		var sa sattr
		var verf *[8]byte
		if how == 2 { // EXCLUSIVE
			var v [8]byte
			copy(v[:], r.fixed(8))
			verf = &v
		} else {
			sa = r.sattr()
		}
		if r.err != nil {
			return 0, false
		}
		if hs != proto.OK {
			return fail(hs)
		}
		mode := uint32(0o644)
		if sa.mode != nil {
			mode = *sa.mode
		}
		trunc := sa.size != nil && *sa.size == 0
		a, st := fs.Create(dir, name, mode, how != 0, trunc, verf)
		if st == proto.OK && sa.size != nil && *sa.size != 0 {
			a, st = fs.SetAttr(a.FileID, sa.size, nil)
		}
		if st != proto.OK {
			return fail(st)
		}
		s.created(w, &a)

	case 9: // MKDIR
		dir, hs := s.fh(r)
		name := r.str(4096)
		sa := r.sattr()
		if r.err != nil {
			return 0, false
		}
		if hs != proto.OK {
			return fail(hs)
		}
		mode := uint32(0o755)
		if sa.mode != nil {
			mode = *sa.mode
		}
		a, st := fs.Mkdir(dir, name, mode)
		if st != proto.OK {
			return fail(st)
		}
		s.created(w, &a)

	case 10: // SYMLINK
		dir, hs := s.fh(r)
		name := r.str(4096)
		r.sattr()
		target := r.str(4096)
		if r.err != nil {
			return 0, false
		}
		if hs != proto.OK {
			return fail(hs)
		}
		a, st := fs.Symlink(dir, name, target)
		if st != proto.OK {
			return fail(st)
		}
		s.created(w, &a)

	case 11, 15: // MKNOD, LINK
		return fail(proto.ErrNotSupp)

	case 12, 13: // REMOVE, RMDIR
		dir, hs := s.fh(r)
		name := r.str(4096)
		if r.err != nil {
			return 0, false
		}
		if hs != proto.OK {
			return fail(hs)
		}
		var st proto.Status
		if proc == 12 {
			st = fs.Remove(dir, name)
		} else {
			st = fs.Rmdir(dir, name)
		}
		if st != proto.OK {
			return fail(st)
		}
		w.u32(0)
		s.wcc(w, nil, nil)

	case 14: // RENAME
		fdir, hs1 := s.fh(r)
		fname := r.str(4096)
		tdir, hs2 := s.fh(r)
		tname := r.str(4096)
		if r.err != nil {
			return 0, false
		}
		if hs1 != proto.OK {
			return fail(hs1)
		}
		if hs2 != proto.OK {
			return fail(hs2)
		}
		if st := fs.Rename(fdir, fname, tdir, tname); st != proto.OK {
			return fail(st)
		}
		w.u32(0)
		s.wcc(w, nil, nil)
		s.wcc(w, nil, nil)

	case 16, 17: // READDIR, READDIRPLUS
		return s.readdir(proc == 17, r, w, fail)

	case 18: // FSSTAT
		id, hs := s.fh(r)
		if r.err != nil {
			return 0, false
		}
		if hs != proto.OK {
			return fail(hs)
		}
		f, st := fs.Statfs()
		if st != proto.OK {
			return fail(st)
		}
		w.u32(0)
		s.postOpCached(w, id)
		w.u64(f.Blocks * f.Bsize)
		w.u64(f.Bfree * f.Bsize)
		w.u64(f.Bavail * f.Bsize)
		w.u64(f.Files)
		w.u64(f.Ffree)
		w.u64(f.Ffree)
		w.u32(0) // invarsec

	case 19: // FSINFO
		id, hs := s.fh(r)
		if r.err != nil {
			return 0, false
		}
		if hs != proto.OK {
			return fail(hs)
		}
		w.u32(0)
		s.postOpCached(w, id)
		w.u32(maxIO)    // rtmax
		w.u32(prefIO)   // rtpref
		w.u32(4096)     // rtmult
		w.u32(maxIO)    // wtmax
		w.u32(prefIO)   // wtpref
		w.u32(4096)     // wtmult
		w.u32(64 << 10) // dtpref
		w.u64(1<<63 - 1)
		w.u32(0) // time_delta: 1 ns
		w.u32(1)
		w.u32(0x02 | 0x08 | 0x10) // SYMLINK | HOMOGENEOUS | CANSETTIME

	case 20: // PATHCONF
		id, hs := s.fh(r)
		if r.err != nil {
			return 0, false
		}
		if hs != proto.OK {
			return fail(hs)
		}
		w.u32(0)
		s.postOpCached(w, id)
		w.u32(1)   // linkmax
		w.u32(255) // name_max
		w.u32(1)   // no_trunc
		w.u32(1)   // chown_restricted
		w.u32(0)   // case_insensitive
		w.u32(1)   // case_preserving

	case 21: // COMMIT
		id, hs := s.fh(r)
		r.u64()
		r.u32()
		if r.err != nil {
			return 0, false
		}
		if hs != proto.OK {
			return fail(hs)
		}
		if st := fs.Commit(id); st != proto.OK {
			return fail(st)
		}
		w.u32(0)
		w.u32(0) // no pre-op attributes
		s.postOpCached(w, id)
		w.u64(fs.WriteVerf())
	}
	return proto.OK, true
}

// created writes the success body shared by CREATE, MKDIR and SYMLINK.
func (s *Server) created(w *writer, a *vfs.Attr) {
	w.u32(0)
	w.u32(1) // handle follows
	s.putFH(w, a.FileID)
	s.postOp(w, a)
	s.wcc(w, nil, nil)
}

func (s *Server) readdir(plus bool, r *reader, w *writer, fail func(proto.Status) (proto.Status, bool)) (proto.Status, bool) {
	id, hs := s.fh(r)
	cookie := r.u64()
	r.fixed(8) // cookie verifier: positions are stable, so not needed
	if plus {
		r.u32() // dircount
	}
	maxcount := r.u32()
	if r.err != nil {
		return 0, false
	}
	if hs != proto.OK {
		return fail(hs)
	}
	l, st := s.fs.ReadDir(id, cookie)
	if st != proto.OK {
		return fail(st)
	}

	entry := func(i int) (string, *vfs.Attr) {
		switch i {
		case 0:
			return ".", &l.Dot
		case 1:
			return "..", &l.DotDot
		}
		e := &l.Entries[i-2]
		return e.Name, &e.Attr
	}
	size := func(name string) int {
		n := 4 + 8 + 4 + padded(len(name)) + 8
		if plus {
			n += 4 + 84 + 4 + 4 + 16
		}
		return n
	}
	total := len(l.Entries) + 2
	budget := int(min(max(maxcount, 1024), 1<<20)) - (4 + 4 + 84 + 8) - 8
	i := int(min(cookie, uint64(total)))
	if i < total {
		if name, _ := entry(i); size(name) > budget {
			return fail(proto.ErrTooSmall)
		}
	}

	w.u32(0)
	s.postOp(w, &l.Dot)
	w.u64(l.Verf)
	for ; i < total; i++ {
		name, a := entry(i)
		sz := size(name)
		if sz > budget {
			break
		}
		budget -= sz
		w.u32(1) // entry follows
		w.u64(a.FileID)
		w.str(name)
		w.u64(uint64(i + 1)) // cookie: position of the next entry
		if plus {
			s.postOp(w, a)
			w.u32(1)
			s.putFH(w, a.FileID)
		}
	}
	w.u32(0) // no more entries in this reply
	w.boolean(i >= total)
	return proto.OK, true
}
