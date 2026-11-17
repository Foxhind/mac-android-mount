// Package nfs is a small NFSv3 + MOUNTv3 server (RFC 1813) over TCP, meant
// to be mounted by the local macOS NFS client on the loopback interface.
//
// Requests are handled concurrently, one goroutine each, so a slow file
// operation never delays unrelated ones, and the file system underneath
// bounds every operation in time, so every request gets an answer.
package nfs

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/Foxhind/macdroidfs/internal/vfs"
)

const (
	progNFS   = 100003
	progMount = 100005

	maxRecord     = 2 << 20
	maxConcurrent = 128

	acceptSuccess      = 0
	acceptProgUnavail  = 1
	acceptProgMismatch = 2
	acceptProcUnavail  = 3
	acceptGarbageArgs  = 4
)

// Server is an NFSv3 server for one vfs.FS.
type Server struct {
	fs       *vfs.FS
	ln       net.Listener
	uid, gid uint32
	sem      chan struct{}

	// Trace, if set, receives one line per request.
	Trace func(format string, args ...any)

	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
}

// Serve starts a server for fs on an ephemeral loopback port.
func Serve(fs *vfs.FS) (*Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &Server{
		fs:    fs,
		ln:    ln,
		uid:   uint32(os.Getuid()),
		gid:   uint32(os.Getgid()),
		sem:   make(chan struct{}, maxConcurrent),
		conns: map[net.Conn]struct{}{},
	}
	go s.acceptLoop()
	return s, nil
}

// Port returns the loopback TCP port the server listens on.
func (s *Server) Port() int { return s.ln.Addr().(*net.TCPAddr).Port }

// Close stops accepting connections and drops the open ones.
func (s *Server) Close() {
	s.mu.Lock()
	s.closed = true
	conns := s.conns
	s.conns = map[net.Conn]struct{}{}
	s.mu.Unlock()
	s.ln.Close()
	for c := range conns {
		c.Close()
	}
}

func (s *Server) acceptLoop() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			c.Close()
			return
		}
		s.conns[c] = struct{}{}
		s.mu.Unlock()
		go s.serveConn(c)
	}
}

var errTooBig = errors.New("nfs: rpc record too large")

func readRecord(br *bufio.Reader) ([]byte, error) {
	var msg []byte
	for {
		var h [4]byte
		if _, err := io.ReadFull(br, h[:]); err != nil {
			return nil, err
		}
		v := binary.BigEndian.Uint32(h[:])
		n := int(v & 0x7fffffff)
		if len(msg)+n > maxRecord {
			return nil, errTooBig
		}
		off := len(msg)
		if msg == nil {
			msg = make([]byte, n)
		} else {
			msg = append(msg, make([]byte, n)...)
		}
		if _, err := io.ReadFull(br, msg[off:]); err != nil {
			return nil, err
		}
		if v&0x80000000 != 0 {
			return msg, nil
		}
	}
}

func (s *Server) serveConn(c net.Conn) {
	defer func() {
		c.Close()
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
	}()
	br := bufio.NewReaderSize(c, 128<<10)
	var wmu sync.Mutex
	for {
		msg, err := readRecord(br)
		if err != nil {
			return
		}
		s.sem <- struct{}{}
		go func() {
			defer func() { <-s.sem }()
			reply := s.handle(msg)
			if reply == nil {
				return
			}
			wmu.Lock()
			c.SetWriteDeadline(time.Now().Add(30 * time.Second))
			_, err := c.Write(reply)
			wmu.Unlock()
			if err != nil {
				c.Close()
			}
		}()
	}
}

// handle decodes one RPC call and returns the complete reply record.
func (s *Server) handle(msg []byte) (reply []byte) {
	r := &reader{b: msg}
	xid := r.u32()
	mtype := r.u32()
	rpcvers := r.u32()
	prog, vers, proc := r.u32(), r.u32(), r.u32()
	credFlavor := r.u32()
	cred := r.opaque(400)
	r.u32() // verifier flavor
	r.opaque(400)
	if r.err != nil || mtype != 0 {
		return nil
	}

	w := &writer{b: make([]byte, 4, 512)} // room for the record mark
	w.u32(xid)
	w.u32(1) // REPLY
	finish := func() []byte {
		binary.BigEndian.PutUint32(w.b, 0x80000000|uint32(len(w.b)-4))
		return w.b
	}
	if rpcvers != 2 {
		w.u32(1) // MSG_DENIED
		w.u32(0) // RPC_MISMATCH
		w.u32(2)
		w.u32(2)
		return finish()
	}
	w.u32(0) // MSG_ACCEPTED
	w.u32(0) // verifier: AUTH_NONE
	w.u32(0)
	statPos := len(w.b)
	w.u32(acceptSuccess)

	defer func() {
		if p := recover(); p != nil {
			if s.Trace != nil {
				s.Trace("nfs: panic in proc %d: %v", proc, p)
			}
			w.b = w.b[:statPos]
			w.u32(5) // SYSTEM_ERR
			reply = finish()
		}
	}()

	stat := uint32(acceptSuccess)
	switch {
	case prog == progNFS && vers == 3:
		stat = s.nfsProc(proc, r, w)
	case prog == progMount && vers == 3:
		uid, ok := credUID(credFlavor, cred)
		stat = s.mountProc(proc, r, w, uid, ok)
	case prog == progNFS || prog == progMount:
		stat = acceptProgMismatch
	default:
		stat = acceptProgUnavail
	}
	if stat != acceptSuccess {
		w.b = w.b[:statPos]
		w.u32(stat)
		if stat == acceptProgMismatch {
			w.u32(3)
			w.u32(3)
		}
	}
	return finish()
}

// credUID extracts the caller's uid from AUTH_SYS credentials.
func credUID(flavor uint32, body []byte) (uint32, bool) {
	if flavor != 1 {
		return 0, false
	}
	r := &reader{b: body}
	r.u32()       // stamp
	r.opaque(255) // machine name
	uid := r.u32()
	return uid, r.err == nil
}

func (s *Server) mountProc(proc uint32, r *reader, w *writer, uid uint32, haveUID bool) uint32 {
	switch proc {
	case 0: // NULL
	case 1: // MNT
		r.str(1024)
		if r.err != nil {
			return acceptGarbageArgs
		}
		// Only the user running the daemon (or root) may obtain the root
		// handle, which is the capability for everything else.
		if !haveUID || (uid != s.uid && uid != 0) {
			w.u32(13) // MNT3ERR_ACCES
			return acceptSuccess
		}
		w.u32(0)
		s.putFH(w, vfs.RootID)
		w.u32(1) // one auth flavor:
		w.u32(1) // AUTH_SYS
	case 2: // DUMP
		w.u32(0)
	case 3, 4: // UMNT, UMNTALL
	case 5: // EXPORT
		w.u32(0)
	default:
		return acceptProcUnavail
	}
	return acceptSuccess
}
