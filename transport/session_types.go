package transport

import (
	"errors"

	"github.com/p1neappleXpress/OpenFlux/transport/control"
)

// MaxNegotiatedPacket leaves space for the envelope and AES overhead in a
// 65535-byte record.
const MaxNegotiatedPacket = 65000

// ErrNegotiationPending is returned by Session.Send / SendControl before the
// handshake has completed.
var ErrNegotiationPending = errors.New("authenticated peer negotiation is not complete")

// PeerParameters carries the peer's advertised capabilities.
type PeerParameters struct {
	Capabilities  control.Capabilities
	MaxPacketSize int
}

// PeerParameterProvider is implemented by Session and exposes the negotiated
// peer parameters to upper layers (e.g. gVisor link MTU).
type PeerParameterProvider interface {
	PeerParameters() (PeerParameters, bool)
}

// ControlHandler receives decoded control packets from the peer. It runs in
// its own goroutine, so it may block without stalling the receive loop.
type ControlHandler func(subtype control.Subtype, payload []byte)

// SendFill is Send for a packet that is copied straight into the buffer the
// session builds (see FillSender).
func (s *Session) SendFill(size int, fill func(dst []byte)) error {
	raw := getBuf(control.EnvelopeSize + size)
	fill(raw[control.EnvelopeSize:])
	return s.send(raw[control.EnvelopeSize:], raw)
}

// CarrierMTU is the IP packet size the tunnel stack on top of this session
// can use for the connections it opens now: the smallest packet size among the
// carriers data goes over (the live ones of the highest priority; DefaultCarrierMTU for a carrier that declares nothing), within the
// limit this side accepts. A carrier outside that group does not count: traffic
// stays on the carriers the profile and the preferences chose, so a flow is never
// split across carriers by packet size. A packet that later has to go over a carrier
// that cannot take it (the group changed) is refused with *PacketTooLargeError, and the
// stack answers the flow with an ICMP "fragmentation needed" (see tunnel).
func (s *Session) CarrierMTU() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	live := s.liveLinksLocked()
	if len(live) == 0 {
		return DefaultCarrierMTU
	}
	group := topLinks(live)
	mtu := s.params.MaxPacketSize
	for _, l := range group {
		mtu = min(mtu, l.limit())
	}
	return max(mtu, 1280)
}

// limit is the largest packet the carrier of l is known to move.
func (l *transportLink) limit() int {
	if l.mtu > 0 {
		return l.mtu
	}
	return DefaultCarrierMTU
}

// hasBigLinkLocked reports whether any carrier of the session declared a packet
// size above DefaultCarrierMTU. Only then are the limits of the carriers
// enforced: a session of carriers that say nothing keeps sending whatever the
// stack above it produces, as it always did. Caller holds s.mu.
func (s *Session) hasBigLinkLocked() bool {
	for _, l := range s.links {
		if l.mtu > DefaultCarrierMTU {
			return true
		}
	}
	return false
}

// fitLink checks a packet of size n above DefaultCarrierMTU against the carrier it is
// going over: it goes if the carrier moves it, else it is refused with the size the
// carrier does move. It never moves the packet to another carrier: a flow split by
// packet size would be reordered by the difference in their delays.
func fitLink(chosen *transportLink, n int) error {
	if chosen.limit() >= n {
		return nil
	}
	return &PacketTooLargeError{Limit: chosen.limit()}
}

// OnReliableStream implements StreamCarrierer: every carrier data goes over now (the live
// ones of the highest priority) is a CheapBytesCarrier, a plain TCP stream.
func (s *Session) OnReliableStream() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	live := s.liveLinksLocked()
	if len(live) == 0 {
		return false
	}
	for _, l := range topLinks(live) {
		if c, ok := l.raw.(CheapBytesCarrier); !ok || !c.CheapBytes() {
			return false
		}
	}
	return true
}
