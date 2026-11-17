// Package device talks to the on-device agent and turns it into a file
// system backend that fails fast instead of blocking.
package device

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Foxhind/macdroidfs/internal/proto"
)

var (
	ErrOffline  = errors.New("device is offline")
	ErrTimeout  = errors.New("device operation timed out")
	ErrLinkDead = errors.New("device link stopped responding")
)

// After this many operation timeouts in a row (with a live link) the agent
// is presumed stuck and the session is torn down so a fresh one can start.
const maxConsecutiveTimeouts = 4

type resp struct {
	st      proto.Status
	payload []byte
}

// Session multiplexes requests over one agent stream.
//
// Liveness is progress based: the watchdog pings continuously, and if no
// byte at all arrives for linkTimeout the stream is declared dead and every
// pending call fails immediately. A slow but working device therefore never
// trips it, and a silent one is detected within linkTimeout no matter what
// was in flight.
type Session struct {
	rwc         io.ReadWriteCloser
	linkTimeout time.Duration

	wmu sync.Mutex // serialises frame writes

	mu   sync.Mutex
	pend map[uint32]chan resp
	next uint32
	err  error
	done chan struct{}

	lastRecv atomic.Int64
	timeouts atomic.Int32
}

type writeDeadliner interface{ SetWriteDeadline(time.Time) error }

// NewSession takes over a stream whose banner has already been consumed.
func NewSession(rwc io.ReadWriteCloser, linkTimeout time.Duration) *Session {
	s := &Session{
		rwc:         rwc,
		linkTimeout: linkTimeout,
		pend:        make(map[uint32]chan resp),
		done:        make(chan struct{}),
	}
	s.lastRecv.Store(time.Now().UnixNano())
	go s.readLoop()
	go s.watchdog()
	return s
}

// Done is closed when the session has failed or been closed.
func (s *Session) Done() <-chan struct{} { return s.done }

// Err returns why the session ended, or nil while it is alive.
func (s *Session) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Close ends the session and fails every pending call.
func (s *Session) Close() { s.fail(errors.New("session closed")) }

func (s *Session) fail(err error) {
	s.mu.Lock()
	if s.err != nil {
		s.mu.Unlock()
		return
	}
	s.err = err
	s.pend = nil
	close(s.done)
	s.mu.Unlock()
	s.rwc.Close() // unblocks the reader and any stalled writer
}

// stampReader records the time of every successful read so that a large
// frame trickling in still counts as progress.
type stampReader struct{ s *Session }

func (r stampReader) Read(p []byte) (int, error) {
	n, err := r.s.rwc.Read(p)
	if n > 0 {
		r.s.lastRecv.Store(time.Now().UnixNano())
	}
	return n, err
}

func (s *Session) readLoop() {
	r := bufio.NewReaderSize(stampReader{s}, 256<<10)
	for {
		id, st, payload, err := proto.ReadResponse(r)
		if err != nil {
			s.fail(fmt.Errorf("agent stream: %w", err))
			return
		}
		s.mu.Lock()
		ch := s.pend[id]
		delete(s.pend, id)
		s.mu.Unlock()
		if ch != nil {
			ch <- resp{st, payload} // buffered
		}
	}
}

func (s *Session) watchdog() {
	interval := max(s.linkTimeout/4, 200*time.Millisecond)
	t := time.NewTicker(interval)
	defer t.Stop()
	var pinging atomic.Bool
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
		}
		if time.Since(time.Unix(0, s.lastRecv.Load())) > s.linkTimeout {
			s.fail(ErrLinkDead)
			return
		}
		if pinging.CompareAndSwap(false, true) {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), s.linkTimeout)
				s.Call(ctx, proto.OpPing, nil)
				cancel()
				pinging.Store(false)
			}()
		}
	}
}

// Call performs one request. It returns a proto.Status error for failures
// reported by the device, ErrTimeout if ctx expires first, and ErrOffline if
// the stream is (or becomes) dead. It never blocks past those.
func (s *Session) Call(ctx context.Context, op byte, payload []byte) ([]byte, error) {
	ch := make(chan resp, 1)
	s.mu.Lock()
	if s.err != nil {
		s.mu.Unlock()
		return nil, ErrOffline
	}
	s.next++
	id := s.next
	s.pend[id] = ch
	s.mu.Unlock()

	frame := proto.AppendRequest(make([]byte, 0, 9+len(payload)), id, op, payload)
	s.wmu.Lock()
	if d, ok := s.rwc.(writeDeadliner); ok {
		d.SetWriteDeadline(time.Now().Add(s.linkTimeout))
	}
	_, err := s.rwc.Write(frame)
	s.wmu.Unlock()
	if err != nil {
		s.fail(fmt.Errorf("agent stream write: %w", err))
		return nil, ErrOffline
	}

	select {
	case r := <-ch:
		s.timeouts.Store(0)
		if r.st != proto.OK {
			return nil, r.st
		}
		return r.payload, nil
	case <-s.done:
		return nil, ErrOffline
	case <-ctx.Done():
		s.mu.Lock()
		if s.pend != nil {
			delete(s.pend, id)
		}
		s.mu.Unlock()
		if op != proto.OpPing && s.timeouts.Add(1) >= maxConsecutiveTimeouts {
			s.fail(errors.New("agent stopped answering requests"))
		}
		return nil, ErrTimeout
	}
}
