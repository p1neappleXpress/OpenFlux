// Package packettunnel keeps the packet-flow lifetime independent of a relay's
// connection state. It contains no Apple/cgo code so its contract can be tested.
package packettunnel

import (
	"context"
	"sync"
	"time"

	"openflux/transport"
)

const MaxPacketSize = 65535
const QueueBytes = 512 << 10

type Session struct {
	t           transport.Transport
	ctx         context.Context
	cancel      context.CancelFunc
	q           chan []byte
	mu          sync.Mutex
	queuedBytes int
	stopOnce    sync.Once
	// Dropped includes oversized packets; truncation is never allowed.
	dropped uint64
}

func New(t transport.Transport) *Session {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Session{t: t, ctx: ctx, cancel: cancel, q: make(chan []byte, 256)}
	t.Receive(s.Enqueue)
	return s
}

func (s *Session) Start() error             { return s.t.Start() }
func (s *Session) Context() context.Context { return s.ctx }
func (s *Session) Connected() bool          { return s.ctx.Err() == nil && s.t.IsConnected() }
func (s *Session) Send(p []byte) {
	if s.Connected() {
		_ = s.t.Send(p)
	}
}
func (s *Session) Stop() {
	s.stopOnce.Do(func() {
		s.cancel() // Wake readers before waiting for any transport shutdown.
		_ = s.t.Stop()
	})
}

func (s *Session) Enqueue(p []byte) {
	if len(p) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		return
	}
	if len(p) > MaxPacketSize || s.queuedBytes+len(p) > QueueBytes || len(s.q) == cap(s.q) {
		s.dropped++
		return
	}
	s.q <- append([]byte(nil), p...)
	s.queuedBytes += len(p)
}

// Read returns >0 for one complete packet, 0 for transient/no packet/insufficient
// buffer, and -1 ONLY after Stop. Relay reconnects never close the queue/context.
// A timeout avoids busy spinning on an idle or disconnected transport.
func (s *Session) Read(dst []byte) int {
	if s.ctx.Err() != nil {
		return -1
	}
	if len(dst) == 0 {
		return 0
	}
	timer := time.NewTimer(250 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-s.ctx.Done():
		return -1
	case <-timer.C:
		return 0
	case p := <-s.q:
		s.mu.Lock()
		s.queuedBytes -= len(p)
		if len(p) > len(dst) {
			s.dropped++
		}
		s.mu.Unlock()
		if s.ctx.Err() != nil {
			return -1
		}
		if len(p) > len(dst) {
			return 0
		}
		return copy(dst, p)
	}
}
