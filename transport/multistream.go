package transport

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// MultiStreamTransport fans a single logical tunnel out over N inner
// transports (issue #50). Send picks the next healthy stream in round-robin
// order; Receive forwards every frame from every inner stream to a single user
// callback.
//
// The tunnel is the UNION of the inner channels. One inner stream going down
// does not stop the tunnel — subsequent sends simply skip it and the two peers
// keep speaking through the survivors. When it comes back the inner transport
// flips IsConnected() true on its own (each yandex/vyandex session already has
// its own reconnect loop with jittered backoff), and Send starts hitting it
// again automatically on the next round-robin tick.
//
// Ordering: MultiStream does NOT reorder within a single inner stream, but
// packets going out via different streams can arrive in different order to the
// peer. That is fine because the tunnel is TCP-inside-encapsulation: the inner
// TCP handles reordering, and the BatchedTransport above us is order-free (a
// batch decodes into a set of packets, each independently delivered).
type MultiStreamTransport struct {
	streams []Transport
	// Monotonic counter; we wrap with % len(streams) at pick time. Using an
	// atomic here means Send is lock-free on the hot path.
	idx     atomic.Uint64
	running atomic.Bool

	mu     sync.RWMutex
	userCb func([]byte)

	startTime time.Time
}

// NewMultiStreamTransport wraps inners into a single logical transport.
// Callers pass at least one inner; a length-1 slice is legal but pointless
// (main.go avoids it — a single URL builds the inner transport directly).
func NewMultiStreamTransport(inners []Transport) *MultiStreamTransport {
	streams := make([]Transport, len(inners))
	copy(streams, inners)
	return &MultiStreamTransport{
		streams:   streams,
		startTime: time.Now(),
	}
}

// Start brings every inner transport up. If any one fails to Start we roll
// back everything we've already started so the call is atomic — otherwise a
// half-open MultiStream is very confusing to reason about.
func (m *MultiStreamTransport) Start() error {
	m.running.Store(true)
	m.startTime = time.Now()
	for i, s := range m.streams {
		if err := s.Start(); err != nil {
			for j := 0; j < i; j++ {
				_ = m.streams[j].Stop()
			}
			m.running.Store(false)
			return fmt.Errorf("multistream: inner[%d] Start: %w", i, err)
		}
	}
	return nil
}

// Stop stops every inner transport, returning the first error but attempting
// them all.
func (m *MultiStreamTransport) Stop() error {
	m.running.Store(false)
	var firstErr error
	for _, s := range m.streams {
		if err := s.Stop(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Send routes one frame to the next healthy inner stream, cycling from the
// last-picked slot. This gives round-robin among healthy peers AND automatic
// skip when a peer is temporarily disconnected. If none is healthy we return
// an error so the caller — typically BatchedTransport — backs off and TCP
// retransmits the payload.
//
// The single-stream shortcut avoids a needless connectivity check and modulo
// on the very common N=1 path (which main.go doesn't build today, but the
// type is publicly usable).
func (m *MultiStreamTransport) Send(data []byte) error {
	if !m.running.Load() {
		return fmt.Errorf("multistream: not running")
	}
	n := len(m.streams)
	if n == 0 {
		return fmt.Errorf("multistream: no inner streams")
	}
	start := m.idx.Add(1) - 1
	for i := 0; i < n; i++ {
		s := m.streams[(start+uint64(i))%uint64(n)]
		if !s.IsConnected() {
			continue
		}
		if err := s.Send(data); err != nil {
			// A per-stream Send failure (queue full, mid-reconnect) is not
			// fatal for the tunnel: try the next stream. If all N fail we
			// fall through to the "no healthy stream" error and let the
			// caller back off.
			continue
		}
		return nil
	}
	return fmt.Errorf("multistream: no healthy stream (of %d)", n)
}

// Receive registers cb for the union of all inner streams. Frames from any
// stream go up unchanged; the upper layer (BatchedTransport / user callback)
// is responsible for demuxing.
func (m *MultiStreamTransport) Receive(cb func([]byte)) {
	m.mu.Lock()
	m.userCb = cb
	m.mu.Unlock()
	// Wire every inner stream's callback back to us. This is safe to call
	// before or after Start because each inner transport buffers its own state.
	for _, s := range m.streams {
		s.Receive(func(data []byte) {
			m.mu.RLock()
			u := m.userCb
			m.mu.RUnlock()
			if u != nil {
				u(data)
			}
		})
	}
}

// IsConnected reports true when at least one inner stream is up. The tunnel is
// usable while any stream survives.
func (m *MultiStreamTransport) IsConnected() bool {
	for _, s := range m.streams {
		if s.IsConnected() {
			return true
		}
	}
	return false
}

// Stats aggregates counters across inner streams. Connected is the OR over
// per-stream Connected. Uptime is measured from the MultiStream's own Start.
func (m *MultiStreamTransport) Stats() TransportStats {
	var agg TransportStats
	var anyUp bool
	for _, s := range m.streams {
		st := s.Stats()
		agg.BytesSent += st.BytesSent
		agg.BytesReceived += st.BytesReceived
		agg.PacketsSent += st.PacketsSent
		agg.PacketsRecv += st.PacketsRecv
		agg.Reconnects += st.Reconnects
		if st.Connected {
			anyUp = true
		}
	}
	agg.Connected = anyUp
	agg.Uptime = time.Since(m.startTime)
	return agg
}

// Streams exposes the inner transports for a status printer or a test. The
// returned slice is a copy: callers must not mutate the MultiStream's own
// backing array, but the elements are the live inner transports.
func (m *MultiStreamTransport) Streams() []Transport {
	out := make([]Transport, len(m.streams))
	copy(out, m.streams)
	return out
}
