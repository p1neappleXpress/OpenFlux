package tunnel

import (
	"bytes"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"openflux/transport"
)

type pairedTransport struct {
	mu   sync.RWMutex
	peer *pairedTransport
	cb   func([]byte)
}

func newTransportPair() (*pairedTransport, *pairedTransport) {
	a, b := &pairedTransport{}, &pairedTransport{}
	a.peer, b.peer = b, a
	return a, b
}

func (p *pairedTransport) Start() error { return nil }
func (p *pairedTransport) Stop() error  { return nil }
func (p *pairedTransport) Send(data []byte) error {
	p.peer.mu.RLock()
	cb := p.peer.cb
	p.peer.mu.RUnlock()
	if cb != nil {
		cb(append([]byte(nil), data...))
	}
	return nil
}
func (p *pairedTransport) Receive(cb func([]byte)) {
	p.mu.Lock()
	p.cb = cb
	p.mu.Unlock()
}
func (p *pairedTransport) IsConnected() bool               { return true }
func (p *pairedTransport) Stats() transport.TransportStats { return transport.TransportStats{} }

func TestL4UDPDatagramRoundTrip(t *testing.T) {
	for _, codec := range []string{"raw", "batched", "batched-encrypted", "legacy-encrypted", "negotiated"} {
		t.Run(codec, func(t *testing.T) { testL4UDPDatagramRoundTrip(t, codec) })
	}
}

func testL4UDPDatagramRoundTrip(t *testing.T, codec string) {
	localIP := testLANIPv4(t)

	echo, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		t.Fatalf("listen UDP: %v", err)
	}
	defer echo.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := echo.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = echo.WriteToUDP(buf[:n], addr)
		}
	}()

	a, b := newTransportPair()
	wrap := func(inner transport.Transport, exit bool) transport.Transport {
		switch codec {
		case "batched", "batched-encrypted":
			inner = transport.NewBatchedTransport(inner)
		case "legacy-encrypted":
			inner = transport.NewCompressedTransport(inner)
		}
		if codec == "batched-encrypted" || codec == "legacy-encrypted" {
			var err error
			inner, err = transport.NewEncryptedTransport(inner, "integration-test-secret-only", t.Name(), exit)
			if err != nil {
				t.Fatal(err)
			}
		}
		if codec == "negotiated" {
			params := transport.PeerParameters{
				Capabilities:  transport.CapabilityIPv4 | transport.CapabilityTCP | transport.CapabilityUDP,
				MaxPacketSize: 1280,
			}
			sess, err := transport.NewSession(params, exit)
			if err != nil {
				t.Fatal(err)
			}
			if err := sess.AddTransport("primary", inner, "integration-test-secret-only", t.Name(), 100); err != nil {
				t.Fatal(err)
			}
			return sess
		}
		return inner
	}
	clientTransport, exitTransport := wrap(a, false), wrap(b, true)
	errs := make(chan error, 2)
	go func() { errs <- clientTransport.Start() }()
	go func() { errs <- exitTransport.Start() }()
	defer clientTransport.Stop()
	defer exitTransport.Stop()
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	exit := NewTCPTunnelMode(exitTransport, true, ExitModeL4)
	client := NewTCPTunnelMode(clientTransport, false, ExitModeL4)
	defer exit.Close()
	defer client.Close()
	if codec == "negotiated" && (client.tunnelEP.MTU() != 1280 || exit.tunnelEP.MTU() != 1280) {
		t.Fatal("negotiated MTU not applied to gVisor")
	}

	dest := net.JoinHostPort(localIP.String(), fmt.Sprintf("%d", echo.LocalAddr().(*net.UDPAddr).Port))
	conn, err := client.DialUDP(dest)
	if err != nil {
		t.Fatalf("DialUDP: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	for _, size := range []int{0, 12, 1200, 2000} {
		want := bytes.Repeat([]byte{0xa5}, size)
		if _, err := conn.Write(want); err != nil {
			t.Fatalf("write %d: %v", size, err)
		}
		got := make([]byte, 2048)
		n, err := conn.Read(got)
		if err != nil {
			t.Fatalf("read %d: %v", size, err)
		}
		if !bytes.Equal(got[:n], want) {
			t.Fatalf("datagram %d was corrupted (received %d bytes)", size, n)
		}
	}
}

func testLANIPv4(t *testing.T) net.IP {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatalf("interfaces: %v", err)
	}
	for _, iface := range ifaces {
		if iface.Flags&(net.FlagUp|net.FlagLoopback|net.FlagPointToPoint) != net.FlagUp {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ip, _, err := net.ParseCIDR(addr.String())
			if err == nil && ip.To4() != nil && !ip.IsLinkLocalUnicast() {
				return ip.To4()
			}
		}
	}
	t.Skip("no non-loopback IPv4 interface for UDP integration test")
	return nil
}
