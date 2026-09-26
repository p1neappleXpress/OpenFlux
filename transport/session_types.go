package transport

import (
	"errors"

	"openflux/transport/control"
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
