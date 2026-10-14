package tunnel

import (
	"bytes"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/p1neappleXpress/OpenFlux/socks5"
)

func TestParseExitSocks5(t *testing.T) {
	cases := []struct {
		in         string
		addr, user string
		pass       string
		wantErr    bool
	}{
		{in: "127.0.0.1:52867", addr: "127.0.0.1:52867"},
		{in: "socks5://127.0.0.1:1080", addr: "127.0.0.1:1080"},
		{in: "socks5h://proxy.local:1080", addr: "proxy.local:1080"},
		{in: "socks5://u:p%40ss@[::1]:1080", addr: "[::1]:1080", user: "u", pass: "p@ss"},
		{in: "localhost:1080", addr: "localhost:1080"},
		{in: "http://127.0.0.1:8080", wantErr: true},
		{in: "127.0.0.1", wantErr: true},
		{in: "127.0.0.1:0", wantErr: true},
		{in: "127.0.0.1:70000", wantErr: true},
		{in: ":1080", wantErr: true},
	}
	for _, c := range cases {
		up, err := parseExitSocks5(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("%q: want error, got %+v", c.in, up)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if up.addr != c.addr || up.username != c.user || up.password != c.pass {
			t.Errorf("%q: got %+v", c.in, up)
		}
	}
}

// recordingDialer is the upstream proxy's own way out: it dials directly and
// remembers what it was asked for, so a test can tell the proxy was used.
type recordingDialer struct {
	mu  sync.Mutex
	tcp []string
	udp []string
}

func (d *recordingDialer) DialTCP(addr string) (net.Conn, error) {
	d.mu.Lock()
	d.tcp = append(d.tcp, addr)
	d.mu.Unlock()
	return net.DialTimeout("tcp", addr, 5*time.Second)
}

func (d *recordingDialer) DialUDP(addr string) (net.Conn, error) {
	d.mu.Lock()
	d.udp = append(d.udp, addr)
	d.mu.Unlock()
	return net.DialTimeout("udp", addr, 5*time.Second)
}

func (d *recordingDialer) seen() (tcp, udp []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.tcp...), append([]string(nil), d.udp...)
}

func freeTCPAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

// startUpstream runs OpenFlux's own SOCKS5 server as the exit's upstream.
func startUpstream(t *testing.T, user, pass string) (string, *recordingDialer) {
	t.Helper()
	d := &recordingDialer{}
	addr := freeTCPAddr(t)
	srv := socks5.NewSOCKS5Server(addr, d)
	if user != "" {
		srv.SetAuth(user, pass)
	}
	if err := srv.Bind(); err != nil {
		t.Fatal(err)
	}
	go srv.Start()
	t.Cleanup(func() { srv.Close() })
	return addr, d
}

func startTCPEcho(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	return l.Addr().String()
}

func startUDPEcho(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 65535)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			pc.WriteTo(buf[:n], from)
		}
	}()
	return pc.LocalAddr().String()
}

func setExitSocks5(t *testing.T, spec string) {
	t.Helper()
	if err := SetExitSocks5(spec); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { SetExitSocks5("") })
}

func roundTrip(t *testing.T, c net.Conn, msg []byte) {
	t.Helper()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write(msg); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, len(msg)+16)
	n, err := io.ReadAtLeast(c, got, len(msg))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got[:n], msg) {
		t.Fatalf("echo: got %q, want %q", got[:n], msg)
	}
}

func TestExitDialTCPDirectByDefault(t *testing.T) {
	SetExitSocks5("")
	echo := startTCPEcho(t)
	c, err := exitDialTCP(echo)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	roundTrip(t, c, []byte("direct"))
}

func TestExitDialTCPThroughSocks5(t *testing.T) {
	echo := startTCPEcho(t)
	proxy, d := startUpstream(t, "", "")
	setExitSocks5(t, proxy)

	c, err := exitDialTCP(echo)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	roundTrip(t, c, []byte("through the proxy"))
	if tcp, _ := d.seen(); len(tcp) != 1 || tcp[0] != echo {
		t.Fatalf("proxy dialed %v, want [%s]", tcp, echo)
	}
}

func TestExitDialTCPThroughSocks5WithAuth(t *testing.T) {
	echo := startTCPEcho(t)
	proxy, _ := startUpstream(t, "user", "secret")

	setExitSocks5(t, "socks5://user:secret@"+proxy)
	c, err := exitDialTCP(echo)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, c, []byte("authenticated"))
	c.Close()

	SetExitSocks5("socks5://user:wrong@" + proxy)
	if c, err := exitDialTCP(echo); err == nil {
		c.Close()
		t.Fatal("wrong password: want error")
	}

	SetExitSocks5(proxy)
	if c, err := exitDialTCP(echo); err == nil {
		c.Close()
		t.Fatal("no credentials for a proxy that wants them: want error")
	}
}

func TestExitDialTCPProxyDown(t *testing.T) {
	setExitSocks5(t, freeTCPAddr(t))
	if c, err := exitDialTCP("127.0.0.1:9"); err == nil {
		c.Close()
		t.Fatal("proxy down: want error, not a direct connection")
	}
}

func TestExitDialUDPThroughSocks5(t *testing.T) {
	echo := startUDPEcho(t)
	proxy, d := startUpstream(t, "", "")
	setExitSocks5(t, proxy)

	c, err := exitDialUDP(echo)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, msg := range []string{"one", "two", "three"} {
		roundTrip(t, c, []byte(msg))
	}
	if _, udp := d.seen(); len(udp) == 0 || udp[0] != echo {
		t.Fatalf("proxy relayed UDP to %v, want %s", udp, echo)
	}
	if c.RemoteAddr().String() != echo {
		t.Fatalf("RemoteAddr %s, want %s", c.RemoteAddr(), echo)
	}
}

func TestStripSocksUDPHeader(t *testing.T) {
	hdr, err := encodeSocksAddr("10.1.2.3:53")
	if err != nil {
		t.Fatal(err)
	}
	pkt := append(append([]byte{0, 0, 0}, hdr...), "dns"...)
	if p, ok := stripSocksUDPHeader(pkt); !ok || string(p) != "dns" {
		t.Fatalf("got %q %v", p, ok)
	}
	frag := append([]byte(nil), pkt...)
	frag[2] = 1
	if _, ok := stripSocksUDPHeader(frag); ok {
		t.Fatal("fragment: want drop")
	}
	if _, ok := stripSocksUDPHeader(pkt[:6]); ok {
		t.Fatal("truncated: want drop")
	}
}

// TestL4ExitThroughSocks5EndToEnd sends a client's TCP and UDP through the
// tunnel to an L4 exit that has an upstream SOCKS5 proxy, and checks that
// both reach the destination by way of the proxy.
func TestL4ExitThroughSocks5EndToEnd(t *testing.T) {
	lan := testLANIPv4(t)

	tcpEcho, err := net.Listen("tcp4", net.JoinHostPort(lan.String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	defer tcpEcho.Close()
	go func() {
		for {
			c, err := tcpEcho.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	udpEcho, err := net.ListenUDP("udp4", &net.UDPAddr{IP: lan})
	if err != nil {
		t.Fatal(err)
	}
	defer udpEcho.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := udpEcho.ReadFromUDP(buf)
			if err != nil {
				return
			}
			udpEcho.WriteToUDP(buf[:n], from)
		}
	}()

	proxy, d := startUpstream(t, "u", "p")
	setExitSocks5(t, "socks5://u:p@"+proxy)

	a, b := newTransportPair()
	exit := NewTCPTunnelMode(b, true, ExitModeL4)
	client := NewTCPTunnelMode(a, false, ExitModeL4)
	defer exit.Close()
	defer client.Close()

	tcpDest := tcpEcho.Addr().String()
	c, err := client.DialTCP(tcpDest)
	if err != nil {
		t.Fatalf("DialTCP: %v", err)
	}
	big := bytes.Repeat([]byte("openflux"), 64*1024) // 512 KB
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	go c.Write(big)
	got := make([]byte, len(big))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("TCP read: %v", err)
	}
	if !bytes.Equal(got, big) {
		t.Fatal("TCP data corrupted")
	}
	c.Close()

	udpDest := udpEcho.LocalAddr().String()
	u, err := client.DialUDP(udpDest)
	if err != nil {
		t.Fatalf("DialUDP: %v", err)
	}
	defer u.Close()
	roundTrip(t, u, []byte("datagram via the proxy"))

	tcp, udp := d.seen()
	if len(tcp) != 1 || tcp[0] != tcpDest {
		t.Fatalf("proxy dialed TCP %v, want [%s]", tcp, tcpDest)
	}
	if len(udp) == 0 || udp[0] != udpDest {
		t.Fatalf("proxy relayed UDP to %v, want %s", udp, udpDest)
	}
}
