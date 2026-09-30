package l3

import (
	"encoding/binary"
	"sync"
	"sync/atomic"
	"time"
)

// sentFlow is an outbound TCP flow as the host sees it after SNAT: the local
// (client-chosen) port and the remote end.
type sentFlow struct {
	srcPort uint16
	dstIP   uint32
	dstPort uint16
}

// sentFlows remembers the TCP flows a backend sent to the network, so that
// the host's own RSTs for them (it never opened those connections) can be
// told apart from RSTs for the host's real connections.
type sentFlows struct {
	ttl   time.Duration
	mu    sync.RWMutex
	flows map[sentFlow]*atomic.Int64 // last send, unix nanos
}

func newSentFlows(ttl time.Duration) *sentFlows {
	return &sentFlows{ttl: ttl, flows: make(map[sentFlow]*atomic.Int64)}
}

// outboundTCPFlow reads the flow of an outbound TCP packet; false for other
// protocols and for non-first fragments, which carry no ports.
func outboundTCPFlow(pkt []byte) (sentFlow, bool) {
	if len(pkt) < 20 || pkt[0]>>4 != 4 || pkt[9] != 6 {
		return sentFlow{}, false
	}
	if binary.BigEndian.Uint16(pkt[6:8])&0x1fff != 0 {
		return sentFlow{}, false
	}
	ihl := int(pkt[0]&0x0f) * 4
	if ihl < 20 || len(pkt) < ihl+4 {
		return sentFlow{}, false
	}
	return sentFlow{
		srcPort: binary.BigEndian.Uint16(pkt[ihl : ihl+2]),
		dstIP:   binary.BigEndian.Uint32(pkt[16:20]),
		dstPort: binary.BigEndian.Uint16(pkt[ihl+2 : ihl+4]),
	}, true
}

func (s *sentFlows) record(f sentFlow, now time.Time) {
	s.mu.RLock()
	seen := s.flows[f]
	s.mu.RUnlock()
	if seen == nil {
		s.mu.Lock()
		if seen = s.flows[f]; seen == nil {
			seen = new(atomic.Int64)
			s.flows[f] = seen
		}
		s.mu.Unlock()
	}
	seen.Store(now.UnixNano())
}

func (s *sentFlows) contains(f sentFlow, now time.Time) bool {
	s.mu.RLock()
	seen := s.flows[f]
	s.mu.RUnlock()
	return seen != nil && now.Sub(time.Unix(0, seen.Load())) <= s.ttl
}

func (s *sentFlows) expire(now time.Time) {
	s.mu.Lock()
	for f, seen := range s.flows {
		if now.Sub(time.Unix(0, seen.Load())) > s.ttl {
			delete(s.flows, f)
		}
	}
	s.mu.Unlock()
}

func (s *sentFlows) len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.flows)
}
