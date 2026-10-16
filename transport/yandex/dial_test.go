package yandex

import (
	"context"
	"net"
	"strings"
	"testing"
)

func stubLookup(t *testing.T, addrs ...string) {
	t.Helper()
	old := lookupIPAddr
	lookupIPAddr = func(_ context.Context, _ string) ([]net.IPAddr, error) {
		out := make([]net.IPAddr, 0, len(addrs))
		for _, a := range addrs {
			out = append(out, net.IPAddr{IP: net.ParseIP(a)})
		}
		return out, nil
	}
	t.Cleanup(func() { lookupIPAddr = old })
}

// A host that resolves to IPv6 first is still reached over IPv4 when it
// answers there: the browser's spravka is bound to the tunnel's IPv4 address.
func TestDialIPv4FirstPrefersIPv4(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skip("no IPv4 loopback:", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	stubLookup(t, "::1", "127.0.0.1") // the resolver's order: IPv6 first

	conn, err := dialIPv4First(context.Background(), "tcp", net.JoinHostPort("carrier.test", port))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if !strings.HasPrefix(conn.RemoteAddr().String(), "127.0.0.1:") {
		t.Fatalf("connected to %s, want the IPv4 address", conn.RemoteAddr())
	}
}

// IPv6 is the fallback: no IPv4 address answers, the IPv6 one does.
func TestDialIPv4FirstFallsBackToIPv6(t *testing.T) {
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skip("no IPv6 loopback:", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	// Nothing listens on 127.0.0.1:<port>: the IPv4 attempt is refused.
	stubLookup(t, "127.0.0.1", "::1")

	conn, err := dialIPv4First(context.Background(), "tcp", net.JoinHostPort("carrier.test", port))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if !strings.HasPrefix(conn.RemoteAddr().String(), "[::1]:") {
		t.Fatalf("connected to %s, want the IPv6 fallback", conn.RemoteAddr())
	}
}

func TestDialIPv4FirstLeavesLiteralsAlone(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			c.Close()
		}
	}()
	// A literal address never touches the resolver.
	old := lookupIPAddr
	lookupIPAddr = func(context.Context, string) ([]net.IPAddr, error) {
		t.Error("resolver used for a literal address")
		return nil, nil
	}
	defer func() { lookupIPAddr = old }()
	conn, err := dialIPv4First(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
}

// Every client of the carriers must go through the dialer; a plain
// http.Client{} would quietly bring the IPv6-first dial back.
func TestCarrierTransportDialsIPv4First(t *testing.T) {
	tr := carrierTransport("")
	if tr.DialContext == nil {
		t.Fatal("carrier transport has no DialContext")
	}
}
