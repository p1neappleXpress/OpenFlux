package tunnel

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"openflux/socks5"
)

type recordingDialer struct {
	mu    sync.Mutex
	dests []string
}

func (d *recordingDialer) DialTCP(addr string) (net.Conn, error) {
	d.mu.Lock()
	d.dests = append(d.dests, addr)
	d.mu.Unlock()
	return net.DialTimeout("tcp", addr, 5*time.Second)
}

// With --upstream-proxy the l4 exit opens its connections through the
// SOCKS5 proxy instead of dialing the destination itself.
func TestExitDialsThroughUpstreamProxy(t *testing.T) {
	localIP := testLANIPv4(t)
	echo, err := net.Listen("tcp4", net.JoinHostPort(localIP.String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()

	pl, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxyAddr := pl.Addr().String()
	pl.Close()
	rec := &recordingDialer{}
	srv := socks5.NewSOCKS5Server(proxyAddr, rec)
	if err := srv.Bind(); err != nil {
		t.Fatal(err)
	}
	go srv.Start()
	defer srv.Close()

	if err := SetExitUpstream("socks5://" + proxyAddr); err != nil {
		t.Fatal(err)
	}
	defer SetExitUpstream("")

	a, b := newTransportPair()
	exit := NewTCPTunnelMode(b, true, ExitModeL4)
	defer exit.Close()
	client := NewTCPTunnelMode(a, false, ExitModeL4)
	defer client.Close()

	conn, err := client.DialTCP(echo.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(conn, got); err != nil || string(got) != "ping" {
		t.Fatalf("echo through the upstream proxy: %q, %v", got, err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.dests) != 1 || rec.dests[0] != echo.Addr().String() {
		t.Fatalf("proxy saw %v, want one connection to %s", rec.dests, echo.Addr())
	}
}

func TestParseUpstreamProxy(t *testing.T) {
	for spec, want := range map[string]string{
		"127.0.0.1:10808":            "127.0.0.1:10808",
		":10808":                     "127.0.0.1:10808",
		"socks5://u:p@10.0.0.1:1080": "10.0.0.1:1080",
	} {
		addr, _, err := parseSOCKS5URL(spec)
		if err != nil || addr != want {
			t.Fatalf("%q -> %q, %v; want %q", spec, addr, err, want)
		}
	}
	for _, bad := range []string{"http://127.0.0.1:8080", "127.0.0.1", "socks5://"} {
		if _, _, err := parseSOCKS5URL(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	if err := SetExitUpstream("direct"); err != nil || ExitUpstream() != "" {
		t.Fatalf("direct: %v %q", err, ExitUpstream())
	}
}
