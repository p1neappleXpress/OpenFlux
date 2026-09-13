package socks5

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

type dialerFunc func(string) (net.Conn, error)

func (function dialerFunc) DialTCP(address string) (net.Conn, error) {
	return function(address)
}

type testSOCKSServer struct {
	server    *SOCKS5Server
	address   string
	serveDone chan error
	stopOnce  sync.Once
	stopErr   error
}

func startTestSOCKSServer(t *testing.T, dialer Dialer, maxRead int) *testSOCKSServer {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if maxRead > 0 {
		listener = &limitedReadListener{Listener: listener, maxRead: maxRead}
	}
	server := NewSOCKS5Server(address, dialer)
	runtime := &testSOCKSServer{server: server, address: address, serveDone: make(chan error, 1)}
	go func() {
		runtime.serveDone <- server.Serve(listener)
	}()
	t.Cleanup(func() {
		if err := runtime.stop(); err != nil {
			t.Errorf("stop SOCKS5 server: %v", err)
		}
	})
	return runtime
}

func (s *testSOCKSServer) stop() error {
	s.stopOnce.Do(func() {
		s.stopErr = s.server.Close()
		if serveErr := <-s.serveDone; serveErr != nil {
			s.stopErr = errors.Join(s.stopErr, serveErr)
		}
	})
	return s.stopErr
}

func (s *testSOCKSServer) dial(t *testing.T) *net.TCPConn {
	t.Helper()
	conn, err := net.DialTimeout("tcp4", s.address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	tcpConn := conn.(*net.TCPConn)
	if err := tcpConn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tcpConn.Close() })
	return tcpConn
}

type limitedReadListener struct {
	net.Listener
	maxRead int
}

func (l *limitedReadListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &limitedReadConn{Conn: conn, maxRead: l.maxRead}, nil
}

type limitedReadConn struct {
	net.Conn
	maxRead int
}

func (c *limitedReadConn) Read(buffer []byte) (int, error) {
	if len(buffer) > c.maxRead {
		buffer = buffer[:c.maxRead]
	}
	return c.Conn.Read(buffer)
}

func echoDialer(addresses chan<- string) Dialer {
	return dialerFunc(func(address string) (net.Conn, error) {
		if addresses != nil {
			addresses <- address
		}
		server, peer := net.Pipe()
		go func() {
			_, _ = io.Copy(peer, peer)
			_ = peer.Close()
		}()
		return server, nil
	})
}

func TestSOCKS5FragmentedDomainConnectAndRelay(t *testing.T) {
	addresses := make(chan string, 1)
	server := startTestSOCKSServer(t, echoDialer(addresses), 1)
	conn := server.dial(t)

	writeFragments(t, conn, []byte{socksVersion, 2, 0x02, methodNoAuth})
	assertBytes(t, conn, []byte{socksVersion, methodNoAuth})
	writeFragments(t, conn, domainRequest("a", 443))
	assertReplyCode(t, conn, replySucceeded)

	select {
	case address := <-addresses:
		if address != "a:443" {
			t.Fatalf("DialTCP address = %q", address)
		}
	case <-time.After(time.Second):
		t.Fatal("DialTCP was not called")
	}
	payload := []byte("SOCKS5 stream payload")
	if err := writeFull(conn, payload); err != nil {
		t.Fatal(err)
	}
	assertBytes(t, conn, payload)
}

func TestSOCKS5IPv4Connect(t *testing.T) {
	addresses := make(chan string, 1)
	server := startTestSOCKSServer(t, echoDialer(addresses), 0)
	conn := server.dial(t)
	negotiateNoAuth(t, conn)
	request := []byte{socksVersion, commandConnect, 0, addressTypeIPv4, 8, 8, 8, 8, 0, 53}
	if err := writeFull(conn, request); err != nil {
		t.Fatal(err)
	}
	assertReplyCode(t, conn, replySucceeded)
	if address := <-addresses; address != "8.8.8.8:53" {
		t.Fatalf("DialTCP address = %q", address)
	}
}

func TestSOCKS5RejectsUnsupportedAuthentication(t *testing.T) {
	server := startTestSOCKSServer(t, nil, 0)
	tests := [][]byte{
		{socksVersion, 0},
		{socksVersion, 2, 0x01, 0x02},
	}
	for _, greeting := range tests {
		conn := server.dial(t)
		if err := writeFull(conn, greeting); err != nil {
			t.Fatal(err)
		}
		assertBytes(t, conn, []byte{socksVersion, methodNotAccepted})
		_ = conn.Close()
	}
}

func TestSOCKS5RequestFailureReplies(t *testing.T) {
	tests := []struct {
		name    string
		request []byte
		code    byte
	}{
		{name: "bad version", request: []byte{0x04, commandConnect, 0, addressTypeIPv4}, code: replyGeneralFailure},
		{name: "bad reserved byte", request: []byte{socksVersion, commandConnect, 1, addressTypeIPv4}, code: replyGeneralFailure},
		{name: "unsupported command", request: []byte{socksVersion, 0x02, 0, addressTypeIPv4}, code: replyCommandUnsupported},
		{name: "IPv6", request: []byte{socksVersion, commandConnect, 0, addressTypeIPv6}, code: replyAddressUnsupported},
		{name: "unknown address type", request: []byte{socksVersion, commandConnect, 0, 0x7f}, code: replyAddressUnsupported},
		{name: "empty domain", request: []byte{socksVersion, commandConnect, 0, addressTypeDomain, 0}, code: replyAddressUnsupported},
		{name: "zero port", request: domainRequest("example.com", 0), code: replyGeneralFailure},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := startTestSOCKSServer(t, nil, 0)
			conn := server.dial(t)
			negotiateNoAuth(t, conn)
			if err := writeFull(conn, test.request); err != nil {
				t.Fatal(err)
			}
			assertReplyCode(t, conn, test.code)
		})
	}
}

func TestSOCKS5DialFailureReplies(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code byte
	}{
		{name: "generic", err: errors.New("failed"), code: replyHostUnreachable},
		{name: "policy", err: codedDialError{code: replyNotAllowed}, code: replyNotAllowed},
		{name: "connection refused", err: fmt.Errorf("wrapped: %w", codedDialError{code: replyConnectionRefused}), code: replyConnectionRefused},
		{name: "invalid custom code", err: codedDialError{code: replySucceeded}, code: replyHostUnreachable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := startTestSOCKSServer(t, dialerFunc(func(string) (net.Conn, error) {
				return nil, test.err
			}), 0)
			conn := server.dial(t)
			negotiateNoAuth(t, conn)
			if err := writeFull(conn, domainRequest("example.com", 443)); err != nil {
				t.Fatal(err)
			}
			assertReplyCode(t, conn, test.code)
		})
	}
}

type codedDialError struct {
	code byte
}

func (e codedDialError) Error() string         { return "coded dial error" }
func (e codedDialError) SOCKS5ReplyCode() byte { return e.code }

func TestSOCKS5TruncatedRequestDoesNotStopServer(t *testing.T) {
	server := startTestSOCKSServer(t, echoDialer(nil), 0)
	conn := server.dial(t)
	negotiateNoAuth(t, conn)
	if err := writeFull(conn, []byte{socksVersion, commandConnect, 0, addressTypeDomain, 5, 'a'}); err != nil {
		t.Fatal(err)
	}
	if err := conn.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(conn); err != nil {
		t.Fatal(err)
	}

	second := server.dial(t)
	negotiateNoAuth(t, second)
	if err := writeFull(second, domainRequest("example.com", 80)); err != nil {
		t.Fatal(err)
	}
	assertReplyCode(t, second, replySucceeded)
}

func TestSOCKS5CloseStopsListenerAndActiveConnections(t *testing.T) {
	targetPeer := make(chan net.Conn, 1)
	server := startTestSOCKSServer(t, dialerFunc(func(string) (net.Conn, error) {
		serverConn, peer := net.Pipe()
		targetPeer <- peer
		return serverConn, nil
	}), 0)
	conn := server.dial(t)
	negotiateNoAuth(t, conn)
	if err := writeFull(conn, domainRequest("example.com", 443)); err != nil {
		t.Fatal(err)
	}
	assertReplyCode(t, conn, replySucceeded)
	peer := <-targetPeer
	defer peer.Close()

	if err := server.stop(); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 1)
	if _, err := conn.Read(buffer); err == nil {
		t.Fatal("active client connection remained open")
	}
	if _, err := net.DialTimeout("tcp4", server.address, 50*time.Millisecond); err == nil {
		t.Fatal("listener still accepted connections after Close")
	}
	if err := server.server.Close(); err != nil {
		t.Fatalf("second Close() = %v", err)
	}
}

func TestSOCKS5ServeRejectsNilListener(t *testing.T) {
	server := NewSOCKS5Server("127.0.0.1:0", nil)
	if err := server.Serve(nil); err == nil {
		t.Fatal("Serve(nil) unexpectedly succeeded")
	}
}

func TestWriteFullHandlesPartialWrites(t *testing.T) {
	writer := &limitedWriter{maxWrite: 1}
	want := []byte("complete")
	if err := writeFull(writer, want); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(writer.Bytes(), want) {
		t.Fatalf("written data = %q", writer.Bytes())
	}
	if err := writeFull(zeroWriter{}, []byte("x")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("zero-byte write error = %v, want io.ErrShortWrite", err)
	}
}

type limitedWriter struct {
	bytes.Buffer
	maxWrite int
}

func (w *limitedWriter) Write(data []byte) (int, error) {
	if len(data) > w.maxWrite {
		data = data[:w.maxWrite]
	}
	return w.Buffer.Write(data)
}

type zeroWriter struct{}

func (zeroWriter) Write([]byte) (int, error) { return 0, nil }

func negotiateNoAuth(t *testing.T, conn net.Conn) {
	t.Helper()
	if err := writeFull(conn, []byte{socksVersion, 1, methodNoAuth}); err != nil {
		t.Fatal(err)
	}
	assertBytes(t, conn, []byte{socksVersion, methodNoAuth})
}

func domainRequest(domain string, port uint16) []byte {
	request := []byte{socksVersion, commandConnect, 0, addressTypeDomain, byte(len(domain))}
	request = append(request, domain...)
	return append(request, byte(port>>8), byte(port))
}

func assertReplyCode(t *testing.T, conn net.Conn, code byte) {
	t.Helper()
	reply := make([]byte, 10)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatal(err)
	}
	if reply[0] != socksVersion || reply[1] != code || reply[2] != 0 || reply[3] != addressTypeIPv4 {
		t.Fatalf("SOCKS5 reply = %v, want code %d", reply, code)
	}
}

func assertBytes(t *testing.T, reader io.Reader, want []byte) {
	t.Helper()
	got := make([]byte, len(want))
	if _, err := io.ReadFull(reader, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("data = %v, want %v", got, want)
	}
}

func writeFragments(t *testing.T, writer io.Writer, data []byte) {
	t.Helper()
	for _, value := range data {
		if err := writeFull(writer, []byte{value}); err != nil {
			t.Fatal(err)
		}
	}
}
