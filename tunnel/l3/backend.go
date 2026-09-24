package l3

import (
	"fmt"
	"net"
)

var errBackendUnavailable = fmt.Errorf("l3: no raw packet backend on this platform")

// localIPOverride, when set via SetLocalIP, is used as the egress IPv4
// address instead of auto-detecting it.
var localIPOverride string

// SetLocalIP overrides the auto-detected egress IP the L3 raw-socket
// backend uses for SNAT/DNAT and the return-packet filter (the --local-ip
// CLI flag). Must be called before New(). Platform-independent: harmless
// (and unused) on platforms without an L3 backend.
func SetLocalIP(ip string) { localIPOverride = ip }

// resolveEgressIP returns localIPOverride, parsed as an IPv4 address, if
// one was set via SetLocalIP; otherwise it calls detect to auto-detect the
// egress IP. Factored out of the platform-specific backend so the override
// logic itself is testable without a real raw socket.
func resolveEgressIP(detect func() ([4]byte, error)) ([4]byte, error) {
	if localIPOverride == "" {
		return detect()
	}
	ip := net.ParseIP(localIPOverride).To4()
	if ip == nil {
		return [4]byte{}, fmt.Errorf("l3: invalid --local-ip %q", localIPOverride)
	}
	var out [4]byte
	copy(out[:], ip)
	return out, nil
}

// L3Backend is the platform-specific raw IPv4 I/O.
//
// Implementations must deliver only packets addressed to EgressIP().
// Recv invokes cb synchronously from a single goroutine.
type L3Backend interface {
	EgressIP() [4]byte
	Send(pkt []byte) error
	Recv(cb func([]byte))
	Close() error
}
