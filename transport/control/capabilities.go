package control

// Capabilities is the bitmap advertised in a Hello envelope. It lives in the
// control package because it is part of the authenticated wire format, not of
// any specific transport.
type Capabilities uint32

const (
	CapabilityIPv4 Capabilities = 1 << iota
	CapabilityTCP
	CapabilityUDP
	CapabilityWireV3 // retired; rejected by the negotiation layer
	CapabilityICMPErrors
)

// DefaultCapabilities is what a peer advertises when it supports everything
// the current envelope can carry.
const DefaultCapabilities = CapabilityIPv4 | CapabilityTCP | CapabilityUDP | CapabilityICMPErrors
