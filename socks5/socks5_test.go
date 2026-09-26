package socks5

import (
	"bytes"
	"errors"
	"io"
	"net"
	"strconv"
	"testing"
	"time"
)

type unusedDialer struct{}

func (unusedDialer) DialTCP(string) (net.Conn, error) { return nil, nil }

type echoDialer struct{ addr string }

func (d echoDialer) DialTCP(string) (net.Conn, error) { return net.Dial("tcp", d.addr) }

type directDialer struct{}

func (directDialer) DialTCP(address string) (net.Conn, error) { return net.Dial("tcp", address) }
func (directDialer) DialUDP(address string) (net.Conn, error) { return net.Dial("udp", address) }

type tcpOnlyDialer struct{}

func (tcpOnlyDialer) DialTCP(address string) (net.Conn, error) { return nil, net.ErrClosed }

// startEchoServer stands in for "the internet": whatever the SOCKS5 server
// dials, it just echoes back what it's sent.
func startEchoServer(t *testing.T) (addr string, closeFn func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				buf := make([]byte, 64)
				n, err := conn.Read(buf)
				if err == nil {
					conn.Write(buf[:n])
				}
			}()
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for condition")
		}
		time.Sleep(time.Millisecond)
	}
}

func waitForListener(t *testing.T, server *SOCKS5Server) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		server.mu.Lock()
		ready := server.listener != nil
		server.mu.Unlock()
		if ready {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("listener did not start")
		}
		time.Sleep(time.Millisecond)
	}
}

// socks5Handshake drives the client side of the greeting (and, if the server
// picks method 0x02, the username/password subnegotiation), returning the
// open connection and the method the server selected.
func socks5Handshake(t *testing.T, proxyAddr string, offerAuth bool, username, password string) (net.Conn, byte) {
	t.Helper()
	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	methods := []byte{0x00}
	if offerAuth {
		methods = append(methods, 0x02)
	}
	greeting := append([]byte{0x05, byte(len(methods))}, methods...)
	if _, err := conn.Write(greeting); err != nil {
		t.Fatal(err)
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		t.Fatal(err)
	}
	if resp[0] != 0x05 {
		t.Fatalf("unexpected SOCKS version in method reply: %d", resp[0])
	}
	if resp[1] == 0x02 {
		authReq := []byte{0x01, byte(len(username))}
		authReq = append(authReq, username...)
		authReq = append(authReq, byte(len(password)))
		authReq = append(authReq, password...)
		if _, err := conn.Write(authReq); err != nil {
			t.Fatal(err)
		}
	}
	return conn, resp[1]
}

func connectAndEcho(t *testing.T, conn net.Conn, targetAddr string) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(targetAddr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	domain := []byte(host)
	request := []byte{0x05, 0x01, 0x00, 0x03, byte(len(domain))}
	request = append(request, domain...)
	request = append(request, byte(port>>8), byte(port))
	if _, err := conn.Write(request); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 10)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatal(err)
	}
	if reply[1] != 0x00 {
		t.Fatalf("CONNECT reply code = %d, want 0", reply[1])
	}

	payload := []byte("ping")
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	back := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, back); err != nil {
		t.Fatal(err)
	}
	if string(back) != string(payload) {
		t.Fatalf("echo = %q, want %q", back, payload)
	}
}

func TestCloseClosesListener(t *testing.T) {
	server := NewSOCKS5Server("127.0.0.1:0", unusedDialer{})
	result := make(chan error, 1)
	go func() { result <- server.Start() }()

	waitForListener(t, server)

	if err := server.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Start() after Close() error = %v, want net.ErrClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Start() did not return after Close()")
	}
}

func TestCloseBeforeStart(t *testing.T) {
	server := NewSOCKS5Server("127.0.0.1:0", unusedDialer{})
	if err := server.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := server.Start(); err == nil {
		t.Fatal("Start() after Close() unexpectedly succeeded")
	}
}

func TestNoAuthByDefault(t *testing.T) {
	echoAddr, closeEcho := startEchoServer(t)
	defer closeEcho()

	server := NewSOCKS5Server("127.0.0.1:0", echoDialer{addr: echoAddr})
	go server.Start()
	defer server.Close()
	waitForListener(t, server)

	conn, method := socks5Handshake(t, server.listener.Addr().String(), false, "", "")
	defer conn.Close()
	if method != 0x00 {
		t.Fatalf("selected method = %d, want 0 (no auth)", method)
	}
	connectAndEcho(t, conn, echoAddr)

	waitFor(t, time.Second, func() bool { return server.BytesSent() >= 4 && server.BytesReceived() >= 4 })
}

func TestAuthRejectsClientWithoutMethod(t *testing.T) {
	server := NewSOCKS5Server("127.0.0.1:0", unusedDialer{})
	server.SetAuth("alice", "hunter2")
	go server.Start()
	defer server.Close()
	waitForListener(t, server)

	conn, method := socks5Handshake(t, server.listener.Addr().String(), false, "", "")
	defer conn.Close()
	if method != 0xFF {
		t.Fatalf("selected method = %d, want 0xFF (no acceptable methods)", method)
	}
}

func TestAuthRejectsWrongCredentials(t *testing.T) {
	server := NewSOCKS5Server("127.0.0.1:0", unusedDialer{})
	server.SetAuth("alice", "hunter2")
	go server.Start()
	defer server.Close()
	waitForListener(t, server)

	conn, method := socks5Handshake(t, server.listener.Addr().String(), true, "alice", "wrong")
	defer conn.Close()
	if method != 0x02 {
		t.Fatalf("selected method = %d, want 2 (username/password)", method)
	}
	authResp := make([]byte, 2)
	if _, err := io.ReadFull(conn, authResp); err != nil {
		t.Fatal(err)
	}
	if authResp[1] != 0x01 {
		t.Fatalf("auth status = %d, want 1 (failure) for wrong credentials", authResp[1])
	}
}

func TestAuthAcceptsCorrectCredentials(t *testing.T) {
	echoAddr, closeEcho := startEchoServer(t)
	defer closeEcho()

	server := NewSOCKS5Server("127.0.0.1:0", echoDialer{addr: echoAddr})
	server.SetAuth("alice", "hunter2")
	go server.Start()
	defer server.Close()
	waitForListener(t, server)

	conn, method := socks5Handshake(t, server.listener.Addr().String(), true, "alice", "hunter2")
	defer conn.Close()
	if method != 0x02 {
		t.Fatalf("selected method = %d, want 2 (username/password)", method)
	}
	authResp := make([]byte, 2)
	if _, err := io.ReadFull(conn, authResp); err != nil {
		t.Fatal(err)
	}
	if authResp[1] != 0x00 {
		t.Fatalf("auth status = %d, want 0 (success) for correct credentials", authResp[1])
	}
	connectAndEcho(t, conn, echoAddr)
}

func TestConnectRejectsUnsupportedCommand(t *testing.T) {
	server := NewSOCKS5Server("127.0.0.1:0", unusedDialer{})
	go server.Start()
	defer server.Close()
	waitForListener(t, server)

	conn, method := socks5Handshake(t, server.listener.Addr().String(), false, "", "")
	defer conn.Close()
	if method != 0x00 {
		t.Fatalf("selected method = %d, want 0", method)
	}
	// BIND (0x02) with a valid IPv4 address; only CONNECT/UDP ASSOCIATE are implemented.
	request := []byte{0x05, 0x02, 0x00, 0x01, 127, 0, 0, 1, 0x00, 0x50}
	if _, err := conn.Write(request); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 10)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatal(err)
	}
	if reply[1] != replyCommandNotSupported {
		t.Fatalf("reply code = %d, want %d (command not supported)", reply[1], replyCommandNotSupported)
	}
}

func TestParseUDPRequestIPv4(t *testing.T) {
	packet := []byte{0, 0, 0, 1, 1, 1, 1, 1, 0, 53, 0xaa, 0xbb}
	addr, payload, err := parseUDPRequest(packet)
	if err != nil {
		t.Fatalf("parseUDPRequest: %v", err)
	}
	if addr != "1.1.1.1:53" || !bytes.Equal(payload, []byte{0xaa, 0xbb}) {
		t.Fatalf("got addr=%q payload=%x", addr, payload)
	}
}

func TestParseUDPRequestRejectsFragments(t *testing.T) {
	packet := []byte{0, 0, 1, 1, 1, 1, 1, 1, 0, 53}
	if _, _, err := parseUDPRequest(packet); err == nil {
		t.Fatal("expected fragmented UDP request to be rejected")
	}
}

func TestMakeUDPResponse(t *testing.T) {
	addr := &net.UDPAddr{IP: net.IPv4(8, 8, 8, 8), Port: 443}
	got := makeUDPResponse(addr, []byte("x"))
	want := []byte{0, 0, 0, 1, 8, 8, 8, 8, 1, 187, 'x'}
	if !bytes.Equal(got, want) {
		t.Fatalf("response=%x want=%x", got, want)
	}
}

func TestIPv6AddressEncoding(t *testing.T) {
	addr := &net.UDPAddr{IP: net.ParseIP("2001:db8::1"), Port: 443}
	packet := makeUDPResponse(addr, []byte("ipv6"))
	got, payload, err := parseUDPRequest(packet)
	if err != nil || got != "[2001:db8::1]:443" || string(payload) != "ipv6" {
		t.Fatalf("IPv6 roundtrip: %s %q %v", got, payload, err)
	}
	var reply bytes.Buffer
	if err := writeReply(&reply, 0, addr); err != nil {
		t.Fatal(err)
	}
	if reply.Bytes()[3] != 4 || reply.Len() != 22 {
		t.Fatalf("bad IPv6 reply: %x", reply.Bytes())
	}
}

func TestTCPOnlyDialerRejectsUDP(t *testing.T) {
	s := NewSOCKS5Server("", tcpOnlyDialer{})
	server, client := net.Pipe()
	defer client.Close()
	done := make(chan struct{})
	go func() { defer close(done); s.handleConnection(server) }()
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = client.Write([]byte{5, 1, 0})
	var method [2]byte
	if _, err := io.ReadFull(client, method[:]); err != nil {
		t.Fatal(err)
	}
	_, _ = client.Write([]byte{5, 3, 0, 1, 0, 0, 0, 0, 0, 0})
	var reply [10]byte
	if _, err := io.ReadFull(client, reply[:]); err != nil {
		t.Fatal(err)
	}
	if reply[1] != 7 {
		t.Fatalf("reply=%x", reply)
	}
	<-done
}

func TestUDPAssociateRoundTrip(t *testing.T) {
	echo, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen echo: %v", err)
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

	server := NewSOCKS5Server("127.0.0.1:0", directDialer{})
	if err := server.Bind(); err != nil {
		t.Fatalf("bind SOCKS: %v", err)
	}
	serverAddr := server.listener.Addr().String()
	go func() { _ = server.Start() }()
	defer server.Close()

	control, err := net.Dial("tcp", serverAddr)
	if err != nil {
		t.Fatalf("dial SOCKS: %v", err)
	}
	defer control.Close()
	_ = control.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := control.Write([]byte{5, 1, 0}); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	method := make([]byte, 2)
	if _, err := io.ReadFull(control, method); err != nil || !bytes.Equal(method, []byte{5, 0}) {
		t.Fatalf("method reply=%x err=%v", method, err)
	}
	if _, err := control.Write([]byte{5, 3, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		t.Fatalf("UDP associate: %v", err)
	}
	reply := make([]byte, 10)
	if _, err := io.ReadFull(control, reply); err != nil || reply[1] != 0 {
		t.Fatalf("associate reply=%x err=%v", reply, err)
	}
	relay := &net.UDPAddr{IP: net.IP(reply[4:8]), Port: int(reply[8])<<8 | int(reply[9])}
	client, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen client: %v", err)
	}
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	echoAddr := echo.LocalAddr().(*net.UDPAddr)
	request := []byte{0, 0, 0, 1, 127, 0, 0, 1, byte(echoAddr.Port >> 8), byte(echoAddr.Port)}
	request = append(request, []byte("socks-udp")...)
	if _, err := client.WriteToUDP(request, relay); err != nil {
		t.Fatalf("write relay: %v", err)
	}
	response := make([]byte, 2048)
	n, _, err := client.ReadFromUDP(response)
	if err != nil {
		t.Fatalf("read relay: %v", err)
	}
	_, payload, err := parseUDPRequest(response[:n])
	if err != nil || string(payload) != "socks-udp" {
		t.Fatalf("response payload=%q err=%v", payload, err)
	}
}
