package transport

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultPeerTimeout is how long a stream may go without hearing from the peer
// before MultiStreamTransport stops routing new traffic to it. Yandex peers
// exchange a keepalive every 10s, so this tolerates one lost keepalive.
const DefaultPeerTimeout = 25 * time.Second

// followSlots is the size of the table that remembers, per flow, the stream a
// packet of the flow last arrived on. A power of two; two flows sharing a slot
// only cost one of them its hint.
const followSlots = 1 << 12

// MultiStreamTransport fans a single logical tunnel out over N inner
// transports (issue #50), one per document. Each inner transport is a complete
// stack of its own (codec, optional encryption, relay), so every document
// carries exactly the wire format a single-document tunnel would.
//
// Routing is per flow, not per packet. Packets of one TCP connection all go
// over the same document: spreading them round-robin over documents with
// different relay latency makes the inner TCP see constant reordering and
// collapse its window (measured: a single download dropped ~8x with two
// documents). Different connections spread over the documents by hash.
//
// A stream is used only while it is healthy, in this order of preference:
//
//  1. connected, and the peer was heard from within the peer timeout;
//  2. connected (startup before the first keepalive, or every peer silent).
//
// "Connected" alone is not enough: a Yandex session can be up while the peer
// is on another document backend, or has dropped out of the document, and
// then everything sent on it is lost. The peer's keepalives are the signal
// that the document actually reaches the other side.
//
// A flow follows the peer: it is sent over the stream its packets last
// arrived on (while that stream is usable), and only a flow with no recent
// inbound packet is placed by hash. When one side loses a document it moves
// its flows to another one at once, but the other side may not notice the loss
// until the keepalive timeout; without following, it would keep sending the
// flow's ACKs into the lost document and stall the connection. A move costs
// the inner TCP one reordering event, not a stall.
type MultiStreamTransport struct {
	streams     []Transport
	peerTimeout time.Duration
	now         func() time.Time

	// follow[h & (followSlots-1)] = tag(32) | stream+1 (8) | unix seconds (24)
	// of the last packet of flow h received. Lock-free: one atomic word per
	// slot.
	follow [followSlots]atomic.Uint64

	// Round-robin cursor for packets that are not IP (no flow to pin).
	rr      atomic.Uint64
	running atomic.Bool

	mu     sync.RWMutex
	userCb func([]byte)

	startTime atomic.Int64 // unix nanos
}

// NewMultiStreamTransport wraps inners into a single logical transport. A
// length-1 slice is legal but pointless (main.go uses the inner transport
// directly for a single URL).
func NewMultiStreamTransport(inners []Transport) *MultiStreamTransport {
	streams := make([]Transport, len(inners))
	copy(streams, inners)
	m := &MultiStreamTransport{
		streams:     streams,
		peerTimeout: DefaultPeerTimeout,
		now:         time.Now,
	}
	m.startTime.Store(time.Now().UnixNano())
	return m
}

// Start brings every inner transport up. A stream that fails to start is left
// out (it stays disconnected and gets no traffic); Start fails only when no
// stream starts at all, since one dead document must not take the tunnel down.
func (m *MultiStreamTransport) Start() error {
	if len(m.streams) == 0 {
		return fmt.Errorf("multistream: no inner streams")
	}
	m.startTime.Store(time.Now().UnixNano())
	m.running.Store(true)

	var errs []error
	for i, s := range m.streams {
		if err := s.Start(); err != nil {
			log.Printf("multistream: stream %d failed to start: %v", i, err)
			errs = append(errs, fmt.Errorf("stream %d: %w", i, err))
		}
	}
	if len(errs) == len(m.streams) {
		_ = m.Stop()
		return fmt.Errorf("multistream: no stream started: %w", errors.Join(errs...))
	}
	return nil
}

// Stop stops every inner transport concurrently and waits for all of them, so
// the total shutdown time is that of the slowest stream, not the sum.
func (m *MultiStreamTransport) Stop() error {
	m.running.Store(false)
	errs := make([]error, len(m.streams))
	var wg sync.WaitGroup
	for i, s := range m.streams {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = s.Stop()
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// Send routes one packet to its flow's stream, falling over to the next usable
// stream when that one is down or refuses the packet. It fails only when no
// stream takes the packet; the inner TCP then retransmits.
func (m *MultiStreamTransport) Send(data []byte) error {
	if !m.running.Load() {
		return fmt.Errorf("multistream: not running")
	}
	n := uint64(len(m.streams))
	if n == 0 {
		return fmt.Errorf("multistream: no inner streams")
	}

	now := m.now()
	var start uint64
	hint := -1
	if h, ok := FlowHash(data); ok {
		start = h % n
		hint = m.followHint(h, now)
	} else {
		start = (m.rr.Add(1) - 1) % n
	}

	// Pass 0 takes streams that reach the peer, pass 1 the ones that are only
	// connected. Within a pass the stream the flow last arrived on goes first
	// (i == -1), then the hash order.
	for pass := 0; pass < 2; pass++ {
		for i := -1; i < int(n); i++ {
			var idx int
			if i < 0 {
				if hint < 0 {
					continue
				}
				idx = hint
			} else {
				idx = int((start + uint64(i)) % n)
				if idx == hint {
					continue
				}
			}
			s := m.streams[idx]
			st := s.Stats()
			if !st.Connected || m.peerAlive(st, now) != (pass == 0) {
				continue
			}
			if s.Send(data) == nil {
				return nil
			}
		}
	}
	return fmt.Errorf("multistream: no usable stream (of %d)", n)
}

// noteArrival records that a packet of pkt's flow arrived on stream idx.
func (m *MultiStreamTransport) noteArrival(idx int, pkt []byte) {
	h, ok := FlowHash(pkt)
	if !ok {
		return
	}
	v := h>>32<<32 | uint64(idx+1)<<24 | uint64(m.now().Unix())&0xffffff
	m.follow[h&(followSlots-1)].Store(v)
}

// followHint returns the stream flow h last arrived on within the peer
// timeout, or -1.
func (m *MultiStreamTransport) followHint(h uint64, now time.Time) int {
	v := m.follow[h&(followSlots-1)].Load()
	if v == 0 || v>>32 != h>>32 {
		return -1
	}
	age := (uint64(now.Unix()) - v) & 0xffffff
	if time.Duration(age)*time.Second > m.peerTimeout {
		return -1
	}
	idx := int(v>>24&0xff) - 1
	if idx < 0 || idx >= len(m.streams) {
		return -1
	}
	return idx
}

func (m *MultiStreamTransport) peerAlive(st TransportStats, now time.Time) bool {
	return !st.LastRecv.IsZero() && now.Sub(st.LastRecv) <= m.peerTimeout
}

// PeerAlive reports whether stream i is connected and has heard from the peer
// within the peer timeout, i.e. whether it gets first-choice traffic.
func (m *MultiStreamTransport) PeerAlive(i int) bool {
	st := m.streams[i].Stats()
	return st.Connected && m.peerAlive(st, m.now())
}

// Receive registers cb for the union of all inner streams.
func (m *MultiStreamTransport) Receive(cb func([]byte)) {
	m.mu.Lock()
	m.userCb = cb
	m.mu.Unlock()
	for i, s := range m.streams {
		s.Receive(func(data []byte) {
			m.noteArrival(i, data)
			m.mu.RLock()
			u := m.userCb
			m.mu.RUnlock()
			if u != nil {
				u(data)
			}
		})
	}
}

// IsConnected reports true when at least one inner stream is up.
func (m *MultiStreamTransport) IsConnected() bool {
	for _, s := range m.streams {
		if s.IsConnected() {
			return true
		}
	}
	return false
}

// Stats aggregates counters across inner streams. Connected is the OR and
// LastRecv the latest over the streams; Uptime runs from the MultiStream's own
// Start.
func (m *MultiStreamTransport) Stats() TransportStats {
	var agg TransportStats
	for _, s := range m.streams {
		st := s.Stats()
		agg.BytesSent += st.BytesSent
		agg.BytesReceived += st.BytesReceived
		agg.PacketsSent += st.PacketsSent
		agg.PacketsRecv += st.PacketsRecv
		agg.Reconnects += st.Reconnects
		agg.Connected = agg.Connected || st.Connected
		if st.LastRecv.After(agg.LastRecv) {
			agg.LastRecv = st.LastRecv
		}
	}
	agg.Uptime = time.Since(time.Unix(0, m.startTime.Load()))
	return agg
}

// Streams exposes the inner transports for a status printer or a test. The
// slice is a copy; the elements are the live inner transports.
func (m *MultiStreamTransport) Streams() []Transport {
	out := make([]Transport, len(m.streams))
	copy(out, m.streams)
	return out
}
