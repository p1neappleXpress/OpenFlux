package tunnel

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/proxy"
	"openflux/socks5"
	"openflux/transport"
)

type mockTransport struct {
	peer *mockTransport
	recv func([]byte)
}

func (m *mockTransport) Start() error                     { return nil }
func (m *mockTransport) Stop() error                      { return nil }
func (m *mockTransport) IsConnected() bool                { return true }
func (m *mockTransport) Stats() transport.TransportStats  { return transport.TransportStats{} }
func (m *mockTransport) Send(data []byte) error {
	if m.peer != nil && m.peer.recv != nil {
		m.peer.recv(data)
	}
	return nil
}
func (m *mockTransport) Receive(handler func([]byte)) {
	m.recv = handler
}

// Simple SOCKS5 server for test
func startTestSOCKS5Server(t *testing.T) (string, func()) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start test socks5: %v", err)
	}

	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go handleTestSocks(conn)
		}
	}()

	return l.Addr().String(), func() { _ = l.Close() }
}

func handleTestSocks(c net.Conn) {
	defer c.Close()
	buf := make([]byte, 256)
	// Handshake
	if _, err := io.ReadFull(c, buf[:2]); err != nil || buf[0] != 0x05 {
		return
	}
	numMethods := int(buf[1])
	if _, err := io.ReadFull(c, buf[:numMethods]); err != nil {
		return
	}
	c.Write([]byte{0x05, 0x00}) // NO AUTH

	// Request
	if _, err := io.ReadFull(c, buf[:4]); err != nil || buf[1] != 0x01 {
		return
	}
	var targetAddr string
	switch buf[3] {
	case 0x01: // IPv4
		if _, err := io.ReadFull(c, buf[:6]); err != nil {
			return
		}
		ip := net.IP(buf[:4])
		port := int(buf[4])<<8 | int(buf[5])
		targetAddr = fmt.Sprintf("%s:%d", ip.String(), port)
	default:
		return
	}
	_ = targetAddr

	// Redirect any target to local echo server for test
	destConn, err := net.DialTimeout("tcp", testEchoAddr.String(), 3*time.Second)
	if err != nil {
		c.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	defer destConn.Close()
	c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})

	errChan := make(chan error, 2)
	go func() {
		_, err := io.Copy(destConn, c)
		errChan <- err
	}()
	go func() {
		_, err := io.Copy(c, destConn)
		errChan <- err
	}()
	<-errChan
}

var testEchoAddr *net.TCPAddr

func TestUpstreamSOCKS5Integration(t *testing.T) {
	// 1. Target Echo Server
	echoListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listener: %v", err)
	}
	defer echoListener.Close()
	testEchoAddr = echoListener.Addr().(*net.TCPAddr)

	go func() {
		for {
			c, err := echoListener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				io.Copy(conn, conn)
			}(c)
		}
	}()

	// 2. Upstream SOCKS5 proxy
	socksAddr, closeSocks := startTestSOCKS5Server(t)
	defer closeSocks()

	// 3. Connect mock transports
	transClient := &mockTransport{}
	transExit := &mockTransport{}
	transClient.peer = transExit
	transExit.peer = transClient

	// 4. Start Exit Node with upstream proxy
	tunExit := NewTCPTunnel(transExit, true, socksAddr)
	if tunExit == nil {
		t.Fatalf("Failed to create exit node tunnel")
	}

	// 5. Start Client tunnel
	tunClient := NewTCPTunnel(transClient, false, "")

	// 6. Connect via client tunnel to target public IP
	target := fmt.Sprintf("1.2.3.4:%d", testEchoAddr.Port)
	conn, err := tunClient.DialTCP(target)
	if err != nil {
		t.Fatalf("DialTCP through tunnel: %v", err)
	}
	defer conn.Close()

	// Test dialer return type directly
	dialer, _ := proxy.SOCKS5("tcp", socksAddr, nil, proxy.Direct)
	testConn, err := dialer.Dial("tcp", target)
	if err == nil {
		_, ok := testConn.(*net.TCPConn)
		t.Logf("proxy.SOCKS5 Dial returned type %T, is *net.TCPConn: %v", testConn, ok)
		testConn.Close()
	}

	msg := "Hello via Upstream SOCKS5 in OpenFlux!"
	if _, err := conn.Write([]byte(msg)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	buf := make([]byte, len(msg)+50)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if string(buf[:n]) != msg {
		t.Fatalf("got %q, want %q", string(buf[:n]), msg)
	}

	t.Logf("Success! Data cleanly passed through client -> mock transport -> exit node -> upstream SOCKS5 -> echo server: %q", string(buf[:n]))
}

func TestUpstreamSOCKS5LargePayload(t *testing.T) {
	// Target Echo Server
	echoListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listener: %v", err)
	}
	defer echoListener.Close()
	echoAddr := echoListener.Addr().(*net.TCPAddr)
	testEchoAddr = echoAddr

	go func() {
		for {
			c, err := echoListener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				io.Copy(conn, conn)
			}(c)
		}
	}()

	socksAddr, closeSocks := startTestSOCKS5Server(t)
	defer closeSocks()

	transClient := &mockTransport{}
	transExit := &mockTransport{}
	transClient.peer = transExit
	transExit.peer = transClient

	tunExit := NewTCPTunnel(transExit, true, socksAddr)
	if tunExit == nil {
		t.Fatalf("Failed to create exit node tunnel")
	}
	tunClient := NewTCPTunnel(transClient, false, "")

	target := fmt.Sprintf("1.2.3.4:%d", echoAddr.Port)
	conn, err := tunClient.DialTCP(target)
	if err != nil {
		t.Fatalf("DialTCP: %v", err)
	}
	defer conn.Close()

	// Send 512 KB
	payloadSize := 512 * 1024
	sendData := make([]byte, payloadSize)
	for i := range sendData {
		sendData[i] = byte(i % 256)
	}

	done := make(chan error, 1)
	go func() {
		recvData := make([]byte, payloadSize)
		_, err := io.ReadFull(conn, recvData)
		if err != nil {
			done <- fmt.Errorf("read: %w", err)
			return
		}
		for i := range sendData {
			if sendData[i] != recvData[i] {
				done <- fmt.Errorf("byte %d mismatch: got %d, want %d", i, recvData[i], sendData[i])
				return
			}
		}
		done <- nil
	}()

	if _, err := conn.Write(sendData); err != nil {
		t.Fatalf("Write: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("large payload test failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("timeout waiting for large payload")
	}
}

func TestFullSocks5Chain(t *testing.T) {
	// 1. Real HTTP server on loopback
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("HTTP OK Response"))
	}))
	defer httpServer.Close()

	httpURL, _ := url.Parse(httpServer.URL)
	_, httpPortStr, _ := net.SplitHostPort(httpURL.Host)
	httpPort, _ := strconv.Atoi(httpPortStr)

	// Target that client will dial (dummy public IP 1.2.3.4:httpPort)
	dummyTargetHost := "1.2.3.4"
	dummyTarget := fmt.Sprintf("%s:%d", dummyTargetHost, httpPort)

	// 2. Upstream SOCKS5 proxy that actually dials the target!
	socksListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("socksListener: %v", err)
	}
	defer socksListener.Close()

	go func() {
		for {
			c, err := socksListener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				buf := make([]byte, 256)
				if _, err := io.ReadFull(conn, buf[:2]); err != nil {
					return
				}
				nMethods := int(buf[1])
				if _, err := io.ReadFull(conn, buf[:nMethods]); err != nil {
					return
				}
				conn.Write([]byte{0x05, 0x00}) // No auth

				if _, err := io.ReadFull(conn, buf[:4]); err != nil || buf[1] != 0x01 {
					return
				}
				var target string
				switch buf[3] {
				case 0x01: // IPv4
					if _, err := io.ReadFull(conn, buf[:6]); err != nil {
						return
					}
					ip := net.IP(buf[:4])
					port := int(buf[4])<<8 | int(buf[5])
					target = fmt.Sprintf("%s:%d", ip.String(), port)
				case 0x03: // Domain
					dLen := make([]byte, 1)
					if _, err := io.ReadFull(conn, dLen); err != nil {
						return
					}
					domain := make([]byte, dLen[0])
					if _, err := io.ReadFull(conn, domain); err != nil {
						return
					}
					portBuf := make([]byte, 2)
					if _, err := io.ReadFull(conn, portBuf); err != nil {
						return
					}
					port := int(portBuf[0])<<8 | int(portBuf[1])
					target = fmt.Sprintf("%s:%d", string(domain), port)
				default:
					return
				}

				if target == dummyTarget {
					target = httpServer.Listener.Addr().String()
				}
				dest, err := net.Dial("tcp", target)
				if err != nil {
					conn.Write([]byte{0x05, 0x04, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
					return
				}
				defer dest.Close()
				conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})

				var wg sync.WaitGroup
				wg.Add(2)
				go func() {
					defer wg.Done()
					defer dest.Close()
					io.Copy(dest, conn)
				}()
				go func() {
					defer wg.Done()
					defer conn.Close()
					io.Copy(conn, dest)
				}()
				wg.Wait()
			}(c)
		}
	}()

	upstreamSocksAddr := socksListener.Addr().String()

	// 3. Mock transports
	transClient := &mockTransport{}
	transExit := &mockTransport{}
	transClient.peer = transExit
	transExit.peer = transClient

	// 4. Exit node
	tunExit := NewTCPTunnel(transExit, true, upstreamSocksAddr)
	if tunExit == nil {
		t.Fatalf("Exit node nil")
	}

	// 5. Client node
	tunClient := NewTCPTunnel(transClient, false, "")

	// 6. Client SOCKS5 server
	clientSocksListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("clientSocksListener: %v", err)
	}
	defer clientSocksListener.Close()
	clientSocksAddr := clientSocksListener.Addr().String()
	clientSocksListener.Close() // close so SOCKS5Server can bind it

	clientSocksServer := socks5.NewSOCKS5Server(clientSocksAddr, tunClient)
	go clientSocksServer.Start()
	time.Sleep(50 * time.Millisecond)

	// 7. Make HTTP request via client SOCKS5 server!
	proxyURL, err := url.Parse("socks5://" + clientSocksAddr)
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}

	httpClient := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		},
		Timeout: 5 * time.Second,
	}

	// Request to dummyTarget
	resp, err := httpClient.Get("http://" + dummyTarget)
	if err != nil {
		t.Fatalf("HTTP GET via SOCKS5: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	if string(body) != "HTTP OK Response" {
		t.Fatalf("got body %q, want %q", string(body), "HTTP OK Response")
	}
	t.Logf("Success! HTTP response: %s", string(body))
}

func TestUpstreamFailureRejection(t *testing.T) {
	// Verify that if upstream target cannot be reached (e.g. Android probing 1.1.1.1:853 DoT on an unsupported proxy),
	// the exit node rejects the request with RST and the client fails promptly instead of hanging indefinitely.
	socksListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("socksListener: %v", err)
	}
	defer socksListener.Close()

	go func() {
		for {
			c, err := socksListener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				buf := make([]byte, 256)
				if _, err := conn.Read(buf); err != nil {
					return
				}
				conn.Write([]byte{0x05, 0x00})
				n, err := conn.Read(buf)
				if err != nil || n < 10 {
					return
				}
				// Simulate upstream proxy rejecting connection immediately (e.g. connection refused)
				conn.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
			}(c)
		}
	}()

	transClient := &mockTransport{}
	transExit := &mockTransport{}
	transClient.peer = transExit
	transExit.peer = transClient

	tunExit := NewTCPTunnel(transExit, true, socksListener.Addr().String())
	if tunExit == nil {
		t.Fatalf("Exit node nil")
	}
	tunClient := NewTCPTunnel(transClient, false, "")

	start := time.Now()
	// Dial an address (simulating Android DoT to 1.1.1.1:853)
	conn, err := tunClient.DialTCP("1.1.1.1:853")
	duration := time.Since(start)

	if err == nil {
		conn.Close()
		t.Fatalf("expected error dialing unreachable port, but got success")
	}
	t.Logf("DialTCP failed as expected in %v with error: %v", duration, err)
	if duration > 3*time.Second {
		t.Fatalf("expected quick rejection, but took %v", duration)
	}
}

