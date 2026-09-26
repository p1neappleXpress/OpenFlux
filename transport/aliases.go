package transport

import "openflux/transport/control"

// Re-exports for callers that still spell these as transport.Capability*.
//
// The authoritative definitions live in transport/control because
// Capabilities is part of the authenticated negotiation envelope, not of any
// particular Transport implementation.
type Capabilities = control.Capabilities

const (
	CapabilityIPv4       = control.CapabilityIPv4
	CapabilityTCP        = control.CapabilityTCP
	CapabilityUDP        = control.CapabilityUDP
	CapabilityWireV3     = control.CapabilityWireV3
	CapabilityICMPErrors = control.CapabilityICMPErrors
	DefaultCapabilities  = control.DefaultCapabilities
)
