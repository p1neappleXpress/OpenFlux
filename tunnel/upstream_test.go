package tunnel

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func startEchoTCP(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	return ln.Addr().String()
}

func readConnectRequest(br *bufio.Reader) (string, string, map[string]string, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return "", "", nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	parts := strings.SplitN(line, " ", 3)
	if len(parts) != 3 || parts[0] != "CONNECT" {
		return "", "", nil, fmt.Errorf("bad request line %q", line)
	}
	target := parts[1]
	headers := map[string]string{}
	for {
		h, err := br.ReadString('\n')
		if err != nil {
			return "", "", nil, err
		}
		h = strings.TrimRight(h, "\r\n")
		if h == "" {
			break
		}
		if k, v, ok := strings.Cut(h, ":"); ok {
			headers[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
	return target, headers["proxy-authorization"], headers, nil
}

func TestParseUpstreamProxy(t *testing.T) {
	if got, err := ParseUpstreamProxy(""); err != nil || got != nil {
		t.Fatalf("empty: got %v, err %v", got, err)
	}
	p, err := ParseUpstreamProxy("127.0.0.1:10808")
	if err != nil || p.Addr != "127.0.0.1:10808" {
		t.Fatalf("bare host:port: %v %v", p, err)
	}
	p, err = ParseUpstreamProxy("http://127.0.0.1:10808")
	if err != nil || p.Addr != "127.0.0.1:10808" {
		t.Fatalf("http URL: %v %v", p, err)
	}
	p, err = ParseUpstreamProxy("http://user:pass@127.0.0.1:10808")
	if err != nil || p.Username != "user" || p.Password != "pass" {
		t.Fatalf("auth: %v %v", p, err)
	}
	if s := p.StringRedacted(); strings.Contains(s, "pass") {
		t.Fatalf("redacted string leaks password: %q", s)
	}
	if _, err := ParseUpstreamProxy("socks5://127.0.0.1:1080"); err == nil {
		t.Fatal("socks5 scheme must be rejected")
	}
	if _, err := ParseUpstreamProxy("https://127.0.0.1:10808"); err == nil {
		t.Fatal("https scheme must be rejected (plain HTTP proxy only)")
	}
	if _, err := ParseUpstreamProxy("http://:10808"); err == nil {
		t.Fatal("missing host must be rejected")
	}
	if _, err := ParseUpstreamProxy("http://127.0.0.1"); err == nil {
		t.Fatal("missing port must be rejected")
	}
}

func TestUpstreamDialSuccess(t *testing.T) {
	echo := startEchoTCP(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				br := bufio.NewReader(c)
				target, _, _, err := readConnectRequest(br)
				if err != nil {
					return
				}
				if target != echo {
					fmt.Fprintf(c, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
					return
				}
				up, err := net.Dial("tcp", echo)
				if err != nil {
					fmt.Fprintf(c, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
					return
				}
				defer up.Close()
				fmt.Fprintf(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
				go func() { _, _ = io.Copy(up, br) }()
				_, _ = io.Copy(c, up)
			}()
		}
	}()
	p, err := ParseUpstreamProxy("http://" + ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn, err := p.Dial("tcp", echo)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	msg := []byte("hello-upstream")
	if _, err := conn.Write(msg); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, got); err != nil || !bytes.Equal(got, msg) {
		t.Fatalf("echo %q, %v", got, err)
	}
}

func TestUpstreamDialProxyErrorStatus(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				br := bufio.NewReader(c)
				if _, _, _, err := readConnectRequest(br); err != nil {
					return
				}
				fmt.Fprintf(c, "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n")
			}()
		}
	}()
	p, _ := ParseUpstreamProxy(ln.Addr().String())
	if _, err := p.Dial("tcp", "example.com:443"); err == nil {
		t.Fatal("expected error on 403")
	} else if !strings.Contains(err.Error(), "403") {
		t.Fatalf("error should mention status, got %v", err)
	}
}

func TestUpstreamDialUnreachable(t *testing.T) {
	p, _ := ParseUpstreamProxy("http://127.0.0.1:1")
	p.DialTimeout = 2 * time.Second
	p.HandshakeTimeout = 2 * time.Second
	start := time.Now()
	if _, err := p.Dial("tcp", "example.com:443"); err == nil {
		t.Fatal("expected dial error")
	} else if time.Since(start) > 10*time.Second {
		t.Fatalf("unreachable proxy took too long: %v", time.Since(start))
	}
}

func TestUpstreamDialHandshakeTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { time.Sleep(5 * time.Second); c.Close() }()
		}
	}()
	p, _ := ParseUpstreamProxy(ln.Addr().String())
	p.HandshakeTimeout = 300 * time.Millisecond
	start := time.Now()
	if _, err := p.Dial("tcp", "example.com:443"); err == nil {
		t.Fatal("expected timeout")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("timeout not enforced, took %v", d)
	}
}

func TestUpstreamDialBidirectional(t *testing.T) {
	echo := startEchoTCP(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				br := bufio.NewReader(c)
				if _, _, _, err := readConnectRequest(br); err != nil {
					return
				}
				up, err := net.Dial("tcp", echo)
				if err != nil {
					return
				}
				defer up.Close()
				fmt.Fprintf(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
				go func() { _, _ = io.Copy(up, br) }()
				_, _ = io.Copy(c, up)
			}()
		}
	}()
	p, _ := ParseUpstreamProxy(ln.Addr().String())
	conn, err := p.Dial("tcp", echo)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	for i := 0; i < 2; i++ {
		msg := bytes.Repeat([]byte{byte('a' + i)}, 4096)
		if _, err := conn.Write(msg); err != nil {
			t.Fatal(err)
		}
		got := make([]byte, len(msg))
		if _, err := io.ReadFull(conn, got); err != nil || !bytes.Equal(got, msg) {
			t.Fatalf("round %d: %v", i, err)
		}
	}
}

func TestUpstreamDialBufferedBytes(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				br := bufio.NewReader(c)
				if _, _, _, err := readConnectRequest(br); err != nil {
					return
				}
				fmt.Fprintf(c, "HTTP/1.1 200 Connection Established\r\n\r\nHELLO")
				buf := make([]byte, 1024)
				for {
					n, err := br.Read(buf)
					if err != nil {
						return
					}
					if _, err := c.Write(buf[:n]); err != nil {
						return
					}
				}
			}()
		}
	}()
	p, _ := ParseUpstreamProxy(ln.Addr().String())
	conn, err := p.Dial("tcp", "example.com:443")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	got := make([]byte, 5)
	if _, err := io.ReadFull(conn, got); err != nil || string(got) != "HELLO" {
		t.Fatalf("buffered bytes lost: %q %v", got, err)
	}
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	echo := make([]byte, 4)
	if _, err := io.ReadFull(conn, echo); err != nil || string(echo) != "ping" {
		t.Fatalf("post-buffer echo: %q %v", echo, err)
	}
}

func TestUpstreamDialTargetPreserved(t *testing.T) {
	cases := []string{"example.com:443", "1.2.3.4:80", net.JoinHostPort("2001:db8::1", "443")}
	for _, want := range cases {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		gotCh := make(chan string, 1)
		go func() {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
			defer ln.Close()
			br := bufio.NewReader(c)
			target, _, _, err := readConnectRequest(br)
			if err != nil {
				return
			}
			gotCh <- target
			fmt.Fprintf(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
			_, _ = io.Copy(c, br)
		}()
		p, _ := ParseUpstreamProxy(ln.Addr().String())
		conn, err := p.Dial("tcp", want)
		if err != nil {
			t.Fatalf("target %q: %v", want, err)
		}
		conn.Close()
		select {
		case got := <-gotCh:
			if got != want {
				t.Fatalf("target distorted: want %q got %q", want, got)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("target %q: proxy saw no CONNECT", want)
		}
	}
}

func TestUpstreamDialBasicAuth(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("user:s3cret"))
	gotCh := make(chan string, 1)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				br := bufio.NewReader(c)
				_, auth, _, err := readConnectRequest(br)
				if err != nil {
					return
				}
				gotCh <- auth
				if auth != wantAuth {
					fmt.Fprintf(c, "HTTP/1.1 407 Proxy Authentication Required\r\n\r\n")
					return
				}
				fmt.Fprintf(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
				_, _ = io.Copy(c, br)
			}()
		}
	}()
	p, _ := ParseUpstreamProxy("http://user:s3cret@" + ln.Addr().String())
	conn, err := p.Dial("tcp", "example.com:443")
	if err != nil {
		t.Fatalf("auth dial: %v", err)
	}
	conn.Close()
	select {
	case got := <-gotCh:
		if got != wantAuth {
			t.Fatalf("auth header = %q, want %q", got, wantAuth)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no CONNECT seen")
	}
	p2, _ := ParseUpstreamProxy(ln.Addr().String())
	if _, err := p2.Dial("tcp", "example.com:443"); err == nil {
		t.Fatal("expected 407 error without credentials")
	}
}

func TestUpstreamDialContextCancel(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { time.Sleep(5 * time.Second); c.Close() }()
		}
	}()
	p, _ := ParseUpstreamProxy(ln.Addr().String())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.DialContext(ctx, "tcp", "example.com:443"); err == nil {
		t.Fatal("expected context-cancel error")
	}
}

func TestDirectDialPreserved(t *testing.T) {
	echo := startEchoTCP(t)
	conn, err := DirectDial("tcp", echo)
	if err != nil {
		t.Fatalf("DirectDial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte("direct")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 6)
	if _, err := io.ReadFull(conn, got); err != nil || string(got) != "direct" {
		t.Fatalf("direct echo %q %v", got, err)
	}
}

func TestExitWithoutUpstreamDirect(t *testing.T) {
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
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	a, b := newTransportPair()
	exit := NewTCPTunnelMode(b, true, ExitModeL4)
	defer exit.Close()
	client := NewTCPTunnelMode(a, false, ExitModeL4)
	defer client.Close()

	dest := echo.Addr().String()
	conn, err := client.DialTCP(dest)
	if err != nil {
		t.Fatalf("DialTCP: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	msg := []byte("old-behavior")
	if _, err := conn.Write(msg); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, got); err != nil || !bytes.Equal(got, msg) {
		t.Fatalf("echo %q %v", got, err)
	}
}

func TestExitUsesCustomDialer(t *testing.T) {
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
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	a, b := newTransportPair()
	exit := NewTCPTunnelMode(b, true, ExitModeL4)
	defer exit.Close()
	var calls int32
	var lastTarget string
	exit.SetExitDialer(func(network, address string) (net.Conn, error) {
		atomic.AddInt32(&calls, 1)
		lastTarget = address
		return net.DialTimeout(network, address, 5*time.Second)
	})
	client := NewTCPTunnelMode(a, false, ExitModeL4)
	defer client.Close()

	dest := echo.Addr().String()
	conn, err := client.DialTCP(dest)
	if err != nil {
		t.Fatalf("DialTCP: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	msg := []byte("via-dialer")
	if _, err := conn.Write(msg); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, got); err != nil || !bytes.Equal(got, msg) {
		t.Fatalf("echo %q %v", got, err)
	}
	if atomic.LoadInt32(&calls) == 0 {
		t.Fatal("exit did not use the custom dialer")
	}
	if lastTarget != dest {
		t.Fatalf("dialer target = %q, want %q", lastTarget, dest)
	}
}

func TestExitViaUpstreamProxy(t *testing.T) {
	localIP := testLANIPv4(t)
	echo, err := net.Listen("tcp4", net.JoinHostPort(localIP.String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	echoAddr := echo.Addr().String()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var proxyConns int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				br := bufio.NewReader(c)
				target, _, _, err := readConnectRequest(br)
				if err != nil {
					return
				}
				if target != echoAddr {
					fmt.Fprintf(c, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
					return
				}
				atomic.AddInt32(&proxyConns, 1)
				up, err := net.Dial("tcp", echoAddr)
				if err != nil {
					fmt.Fprintf(c, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
					return
				}
				defer up.Close()
				fmt.Fprintf(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
				go func() { _, _ = io.Copy(up, br) }()
				_, _ = io.Copy(c, up)
			}()
		}
	}()
	upstream, err := ParseUpstreamProxy("http://" + ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	a, b := newTransportPair()
	exit := NewTCPTunnelMode(b, true, ExitModeL4)
	exit.SetExitUpstream(upstream)
	defer exit.Close()
	client := NewTCPTunnelMode(a, false, ExitModeL4)
	defer client.Close()

	conn, err := client.DialTCP(echoAddr)
	if err != nil {
		t.Fatalf("DialTCP: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	msg := []byte("exit-via-proxy")
	if _, err := conn.Write(msg); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, got); err != nil || !bytes.Equal(got, msg) {
		t.Fatalf("echo via proxy %q %v", got, err)
	}
	if atomic.LoadInt32(&proxyConns) == 0 {
		t.Fatal("proxy saw no CONNECT: exit did not use upstream")
	}
}

func TestNewExitNodeWithUpstream(t *testing.T) {
	a, b := newTransportPair()
	_ = a
	node, err := NewExitNodeWithUpstream(b, "l4", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	_ = node.Stop()

	up, _ := ParseUpstreamProxy("http://127.0.0.1:10808")
	node2, err := NewExitNodeWithUpstream(b, "l4", up)
	if err != nil {
		t.Fatal(err)
	}
	px, ok := node2.(*proxyExit)
	if !ok {
		t.Fatalf("l4 node = %T, want *proxyExit", node2)
	}
	if px.upstream == nil || px.upstream.Addr != "127.0.0.1:10808" {
		t.Fatalf("upstream not wired: %+v", px.upstream)
	}
	node3, err := NewExitNode(b, "l4")
	if err != nil {
		t.Fatal(err)
	}
	_ = node3.Stop()
}
