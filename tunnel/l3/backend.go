package l3

import (
	"fmt"
	"net"
	"sync"
)

var errBackendUnavailable = fmt.Errorf("l3: no raw packet backend on this platform")

var localIPOverride [4]byte
var hasLocalIPOverride bool
var localIPMu sync.Mutex

// SetLocalIP configures the source address used by the L3 backend. Passing an
// empty string restores automatic egress address detection.
func SetLocalIP(value string) error {
	localIPMu.Lock()
	defer localIPMu.Unlock()
	if value == "" {
		hasLocalIPOverride = false
		localIPOverride = [4]byte{}
		return nil
	}
	ip := net.ParseIP(value).To4()
	if ip == nil {
		return fmt.Errorf("l3: invalid IPv4 local address %q", value)
	}
	copy(localIPOverride[:], ip)
	hasLocalIPOverride = true
	return nil
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
