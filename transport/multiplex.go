package transport

import (
	"fmt"
	"sync/atomic"
	"time"

	"universal-bypass-tool/utils"
)

// MultiplexTransport spreads packets across N independent inner transports
// (e.g. N separate Yandex.Docs documents), widening the aggregate channel
// past the per-connection / per-document throttle of a single covert link.
//
// v1 strategy: per-flow affinity. Every packet is hashed by its IP 5-tuple and
// pinned to one inner channel, so a single TCP flow always travels one link and
// stays in order (no resequencing needed). Distinct flows land on distinct
// channels, so multi-connection traffic (speedtests, browsers) fans out and the
// throughput adds up. A single lone flow does NOT speed up under v1 — that is
// what a future per-packet+seq mode (v2) would address.
//
// The wire is untouched: each inner is an ordinary, unmodified transport, so an
// exit node simply runs the same list of documents and merges their receives.
type MultiplexTransport struct {
	inner []Transport

	cb func([]byte)

	running   atomic.Int32
	startTime time.Time
}

// NewMultiplexTransport wraps the given inner transports into one Transport.
// Order matters: the client and exit node must pass the SAME ordered list so a
// flow that hashes to channel k on one side is read from channel k on the other
// (though correctness does not actually depend on it — each inner is a full
// bidirectional channel; ordering only keeps behavior symmetric).
func NewMultiplexTransport(inner []Transport) Transport {
	return &MultiplexTransport{
		inner:     inner,
		startTime: time.Now(),
	}
}

func (m *MultiplexTransport) Start() error {
	m.running.Store(1)
	m.startTime = time.Now()

	started := 0
	var firstErr error
	for i, t := range m.inner {
		if err := t.Start(); err != nil {
			utils.Debugf("[MUX] channel %d failed to start: %v", i, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		started++
	}
	if started == 0 {
		return fmt.Errorf("multiplex: no channels started (%d configured): %v", len(m.inner), firstErr)
	}
	utils.Debugf("[MUX] started %d/%d channels", started, len(m.inner))
	return nil
}

func (m *MultiplexTransport) Stop() error {
	m.running.Store(0)
	for _, t := range m.inner {
		_ = t.Stop()
	}
	return nil
}

// Send routes one packet to a live channel chosen by its flow hash. Only
// currently-connected channels are eligible, so a channel that is mid-reconnect
// is transparently skipped and its flows rehash onto healthy links.
func (m *MultiplexTransport) Send(data []byte) error {
	t := m.pick(data)
	if t == nil {
		return fmt.Errorf("multiplex: no connected channel")
	}
	return t.Send(data)
}

// pick selects the (flowHash mod liveCount)-th connected channel without
// allocating on the hot path. If nothing is connected it returns nil.
func (m *MultiplexTransport) pick(data []byte) Transport {
	n := len(m.inner)
	if n == 0 {
		return nil
	}

	live := 0
	for _, t := range m.inner {
		if t.IsConnected() {
			live++
		}
	}
	if live == 0 {
		return nil
	}

	target := int(flowHash(data) % uint32(live))
	for _, t := range m.inner {
		if t.IsConnected() {
			if target == 0 {
				return t
			}
			target--
		}
	}
	return nil // unreachable: live > 0 guarantees a hit
}

// Receive fans a single upstream callback out to every channel. Packets arrive
// interleaved from all links; each is a whole IP packet, so the tunnel forwards
// it regardless of which channel delivered it.
func (m *MultiplexTransport) Receive(callback func([]byte)) {
	m.cb = callback
	for _, t := range m.inner {
		t.Receive(callback)
	}
}

// IsConnected reports true if at least one channel is up.
func (m *MultiplexTransport) IsConnected() bool {
	for _, t := range m.inner {
		if t.IsConnected() {
			return true
		}
	}
	return false
}

// Stats aggregates the counters of every channel.
func (m *MultiplexTransport) Stats() TransportStats {
	var agg TransportStats
	for _, t := range m.inner {
		s := t.Stats()
		agg.BytesSent += s.BytesSent
		agg.BytesReceived += s.BytesReceived
		agg.PacketsSent += s.PacketsSent
		agg.PacketsRecv += s.PacketsRecv
		agg.Reconnects += s.Reconnects
		if s.Connected {
			agg.Connected = true
		}
	}
	agg.Uptime = time.Since(m.startTime)
	return agg
}

const (
	fnv32Offset uint32 = 2166136261
	fnv32Prime  uint32 = 16777619
)

// flowHash derives a stable hash from a packet's IPv4 5-tuple (src/dst IP,
// protocol, and src/dst ports for TCP/UDP). All packets of one flow hash equal,
// so per-flow affinity holds. Non-IPv4 or truncated packets fall back to
// hashing the leading header bytes, which still pins a given peer consistently.
func flowHash(pkt []byte) uint32 {
	h := fnv32Offset
	mix := func(b byte) { h = (h ^ uint32(b)) * fnv32Prime }

	if len(pkt) >= 20 && pkt[0]>>4 == 4 {
		ihl := int(pkt[0]&0x0f) * 4
		proto := pkt[9]
		for i := 12; i < 20; i++ { // src IP (12-15) + dst IP (16-19)
			mix(pkt[i])
		}
		mix(proto)
		if (proto == 6 || proto == 17) && len(pkt) >= ihl+4 {
			for i := ihl; i < ihl+4; i++ { // src port + dst port
				mix(pkt[i])
			}
		}
		return fmix32(h)
	}

	n := len(pkt)
	if n > 20 {
		n = 20
	}
	for i := 0; i < n; i++ {
		mix(pkt[i])
	}
	return fmix32(h)
}

// fmix32 is the MurmurHash3 finalizer. FNV's low bits are poorly mixed, so a
// bare `hash % channels` split is lopsided; this avalanche spreads entropy into
// every bit position, giving an even split over any small channel count.
func fmix32(h uint32) uint32 {
	h ^= h >> 16
	h *= 0x85ebca6b
	h ^= h >> 13
	h *= 0xc2b2ae35
	h ^= h >> 16
	return h
}
