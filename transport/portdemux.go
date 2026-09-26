package transport

import (
	"encoding/binary"
	"sync"
)

// PortDemux splits a Transport's inbound IPv4 packets between the main
// consumer and a side stack that owns a reserved range of local TCP ports.
//
// The exit only ever answers one client address (the L3 exit drops other
// sources and rewrites replies to 10.10.10.2), so a second in-process stack
// on the client has to share that address; it is told apart by the local
// ports it is confined to instead.
type PortDemux struct {
	Transport
	lo, hi uint16

	mu   sync.RWMutex
	main func([]byte)
	side func([]byte)
}

// NewPortDemux takes over inner's receive path. TCP packets addressed to a
// local port in [lo, hi] go to Side(); everything else to Receive.
func NewPortDemux(inner Transport, lo, hi uint16) *PortDemux {
	d := &PortDemux{Transport: inner, lo: lo, hi: hi}
	inner.Receive(d.dispatch)
	return d
}

// Receive installs the main consumer.
func (d *PortDemux) Receive(cb func([]byte)) {
	d.mu.Lock()
	d.main = cb
	d.mu.Unlock()
}

// Side returns a Transport for the side stack: it sends through the inner
// transport and receives only the reserved-port traffic.
func (d *PortDemux) Side() Transport { return sideEnd{d} }

func (d *PortDemux) dispatch(p []byte) {
	d.mu.RLock()
	cb := d.main
	if port, ok := tcpDestinationPort(p); ok && port >= d.lo && port <= d.hi {
		cb = d.side
	}
	d.mu.RUnlock()
	if cb != nil {
		cb(p)
	}
}

// tcpDestinationPort returns the destination port of an unfragmented IPv4
// TCP packet.
func tcpDestinationPort(p []byte) (uint16, bool) {
	if len(p) < 20 || p[0]>>4 != 4 || p[9] != 6 {
		return 0, false
	}
	if binary.BigEndian.Uint16(p[6:8])&0x1fff != 0 {
		return 0, false
	}
	ihl := int(p[0]&0x0f) * 4
	if ihl < 20 || len(p) < ihl+4 {
		return 0, false
	}
	return binary.BigEndian.Uint16(p[ihl+2 : ihl+4]), true
}

type sideEnd struct{ d *PortDemux }

func (s sideEnd) Start() error          { return nil }
func (s sideEnd) Stop() error           { return nil }
func (s sideEnd) Send(p []byte) error   { return s.d.Transport.Send(p) }
func (s sideEnd) IsConnected() bool     { return s.d.Transport.IsConnected() }
func (s sideEnd) Stats() TransportStats { return TransportStats{} }
func (s sideEnd) Receive(cb func([]byte)) {
	s.d.mu.Lock()
	s.d.side = cb
	s.d.mu.Unlock()
}
