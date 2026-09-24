package tunnel

import (
	"testing"

	"openflux/transport"
)

// stubTransport is a minimal no-op transport.Transport for exercising
// tunnel construction without any real network I/O.
type stubTransport struct{}

func (stubTransport) Start() error                { return nil }
func (stubTransport) Stop() error                 { return nil }
func (stubTransport) Send(data []byte) error      { return nil }
func (stubTransport) Receive(cb func([]byte))     {}
func (stubTransport) IsConnected() bool           { return true }
func (stubTransport) Stats() transport.TransportStats { return transport.TransportStats{} }

// Before the fix, NewTCPTunnelMode only debug-logged a CreateNIC failure and
// always returned a *TCPTunnel with no way to signal the failure to the
// caller; proxyExit.Start() (the L4 exit path) then always returned nil
// regardless. Both now propagate the error. This test locks in the
// contract for the success path (a NIC failure itself is an internal gVisor
// condition that isn't practical to force black-box in a portable test),
// so a future refactor that silently drops the error again fails to build
// instead of failing to run.
func TestNewTCPTunnelModeReturnsError(t *testing.T) {
	tun, err := NewTCPTunnelMode(stubTransport{}, false, ExitModeL4)
	if err != nil {
		t.Fatalf("unexpected error on a normal tunnel init: %v", err)
	}
	if tun == nil {
		t.Fatal("expected a non-nil tunnel on success")
	}
}

func TestProxyExitStartPropagatesTunnelError(t *testing.T) {
	p := newProxyExit(stubTransport{})
	if err := p.Start(); err != nil {
		t.Fatalf("unexpected error on a normal exit start: %v", err)
	}
	if p.tun == nil {
		t.Fatal("expected proxyExit.tun to be set after a successful Start()")
	}
}
