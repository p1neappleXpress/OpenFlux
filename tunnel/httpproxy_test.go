package tunnel

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"openflux/transport"
)

func startProxy(t *testing.T, dial func(string) (net.Conn, error)) *url.URL {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() { _ = ServeHTTPProxy(ln, dial) }()
	return &url.URL{Scheme: "http", Host: ln.Addr().String()}
}

func TestHTTPProxyConnect(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
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
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	proxy := startProxy(t, func(a string) (net.Conn, error) { return net.Dial("tcp", a) })

	c, err := net.Dial("tcp", proxy.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\nping", echo.Addr(), echo.Addr())
	r := bufio.NewReader(c)
	resp, err := http.ReadResponse(r, nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT: %v %v", resp, err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(r, got); err != nil || string(got) != "ping" {
		t.Fatalf("tunneled bytes %q, %v (data sent with the CONNECT line must not be lost)", got, err)
	}
}

// End to end: a browser pointed at the proxy reaches the Internet through a
// side stack on the tunnel and the exit, sharing the client address with
// the main path and told apart by its reserved ports.
func TestHTTPProxyThroughSideTunnel(t *testing.T) {
	localIP := testLANIPv4(t)
	web := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "via exit")
	}))
	ln, err := net.Listen("tcp4", net.JoinHostPort(localIP.String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	web.Listener = ln
	web.Start()
	defer web.Close()

	a, b := newTransportPair()
	exit := NewTCPTunnelMode(b, true, ExitModeL4)
	defer exit.Close()
	demux := transport.NewPortDemux(a, 12000, 12999)
	mainPackets := 0
	demux.Receive(func([]byte) { mainPackets++ })
	side, err := NewSideTunnel(demux.Side(), 12000, 12999)
	if err != nil {
		t.Fatal(err)
	}
	defer side.Close()

	proxy := startProxy(t, side.DialTCP)
	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(proxy)},
		Timeout:   10 * time.Second,
	}
	resp, err := client.Get(web.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "via exit") {
		t.Fatalf("body = %q", body)
	}
	if mainPackets != 0 {
		t.Fatalf("%d side-stack packets leaked to the main path", mainPackets)
	}
}
