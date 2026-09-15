package socks5

import (
	"crypto/subtle"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"

	"universal-bypass-tool/utils"
)

type Dialer interface {
	DialTCP(address string) (net.Conn, error)
}

type SOCKS5Server struct {
	listenAddr string
	dialer     Dialer
	username   string
	password   string

	mu       sync.Mutex
	listener net.Listener
	closed   bool

	bytesSent     atomic.Int64
	bytesReceived atomic.Int64
}

func NewSOCKS5Server(addr string, dialer Dialer) *SOCKS5Server {
	return &SOCKS5Server{listenAddr: addr, dialer: dialer}
}

// SetAuth requires SOCKS5 username/password authentication (RFC 1929) for
// every connection; call before Start/Bind. An empty username leaves
// authentication disabled (the default), so any client is accepted exactly
// as before.
func (s *SOCKS5Server) SetAuth(username, password string) {
	s.username = username
	s.password = password
}

// BytesSent returns the total bytes relayed from clients to their dialed
// targets (client -> internet) across every connection this server has
// handled, for a live upload-speed indicator.
func (s *SOCKS5Server) BytesSent() int64 { return s.bytesSent.Load() }

// BytesReceived returns the total bytes relayed back from dialed targets to
// clients (internet -> client), for a live download-speed indicator.
func (s *SOCKS5Server) BytesReceived() int64 { return s.bytesReceived.Load() }

// countingWriter tallies bytes as they're written, so io.Copy's running
// total is visible immediately rather than only once the copy (i.e. the
// whole connection) ends.
type countingWriter struct {
	dst     io.Writer
	counter *atomic.Int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.dst.Write(p)
	c.counter.Add(int64(n))
	return n, err
}

// Bind reserves the listen address so callers can detect "address already in
// use" synchronously, before serving. Safe to call once; Start binds lazily if
// it wasn't called.
func (s *SOCKS5Server) Bind() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return net.ErrClosed
	}
	if s.listener != nil {
		return nil
	}
	listener, err := net.Listen("tcp", s.listenAddr)
	if err != nil {
		return err
	}
	s.listener = listener
	return nil
}

func (s *SOCKS5Server) Start() error {
	if err := s.Bind(); err != nil {
		return err
	}

	s.mu.Lock()
	listener := s.listener
	s.mu.Unlock()
	defer listener.Close()

	utils.Debugf("[SOCKS5] Listening on %s", s.listenAddr)

	for {
		conn, err := listener.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				utils.Debugf("[SOCKS5] Listener closed, stopping")
				return net.ErrClosed
			}
			utils.Debugf("[SOCKS5] Accept error: %v", err)
			continue
		}
		go s.handleConnection(conn)
	}
}

// Close stops the server, unblocking Start's accept loop.
func (s *SOCKS5Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.listener != nil {
		return s.listener.Close()
	}
	return nil
}

// SOCKS5 reply codes (RFC 1928 section 6), used for both the CONNECT dial
// outcome and the pre-dial rejections below so a client always gets an
// explicit, standard answer instead of a bare connection close - which most
// SOCKS5 clients read as "still trying" rather than "this failed", and hang
// on rather than fail over or report an error.
const (
	replySucceeded           = 0x00
	replyHostUnreachable     = 0x04
	replyCommandNotSupported = 0x07
	replyAddressNotSupported = 0x08
)

func socks5Reply(code byte) []byte {
	return []byte{0x05, code, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
}

func (s *SOCKS5Server) handleConnection(clientConn net.Conn) {
	// A malformed request must never crash the host process; contain any
	// panic to this connection.
	defer func() {
		if r := recover(); r != nil {
			utils.Debugf("[SOCKS5] Recovered from panic in handler: %v", r)
		}
	}()
	defer clientConn.Close()
	remote := clientConn.RemoteAddr()
	utils.Debugf("[SOCKS5] Accepted connection from %s", remote)

	buf := make([]byte, 256)
	n, err := clientConn.Read(buf)
	if err != nil || n < 2 || buf[0] != 0x05 {
		utils.Debugf("[SOCKS5] %s: bad greeting (n=%d, err=%v)", remote, n, err)
		return
	}
	methodCount := int(buf[1])
	if 2+methodCount > n {
		utils.Debugf("[SOCKS5] %s: truncated method list (methodCount=%d, n=%d)", remote, methodCount, n)
		return
	}
	methods := buf[2 : 2+methodCount]

	if s.username != "" {
		if !containsMethod(methods, 0x02) {
			utils.Debugf("[SOCKS5] %s: client didn't offer username/password auth, rejecting", remote)
			clientConn.Write([]byte{0x05, 0xFF})
			return
		}
		clientConn.Write([]byte{0x05, 0x02})
		if !s.authenticate(clientConn) {
			utils.Debugf("[SOCKS5] %s: authentication failed", remote)
			return
		}
		utils.Debugf("[SOCKS5] %s: authenticated", remote)
	} else {
		clientConn.Write([]byte{0x05, 0x00})
	}

	n, err = clientConn.Read(buf)
	if err != nil || n < 10 {
		utils.Debugf("[SOCKS5] %s: bad request (n=%d, err=%v)", remote, n, err)
		return
	}
	if buf[1] != 0x01 {
		utils.Debugf("[SOCKS5] %s: unsupported command 0x%02x (only CONNECT is implemented)", remote, buf[1])
		clientConn.Write(socks5Reply(replyCommandNotSupported))
		return
	}

	var targetAddr string
	switch buf[3] {
	case 0x01:
		targetAddr = fmt.Sprintf("%d.%d.%d.%d:%d",
			buf[4], buf[5], buf[6], buf[7],
			uint16(buf[8])<<8|uint16(buf[9]))
	case 0x03:
		domainLen := int(buf[4])
		// Bounds-check against what was actually read: address (domainLen
		// bytes) starts at index 5 and is followed by a 2-byte port.
		if domainLen == 0 || 5+domainLen+2 > n {
			utils.Debugf("[SOCKS5] %s: bad domain request (len=%d, n=%d)", remote, domainLen, n)
			return
		}
		targetAddr = fmt.Sprintf("%s:%d",
			string(buf[5:5+domainLen]),
			uint16(buf[5+domainLen])<<8|uint16(buf[6+domainLen]))
	case 0x04:
		utils.Debugf("[SOCKS5] %s: IPv6 CONNECT not supported yet", remote)
		clientConn.Write(socks5Reply(replyAddressNotSupported))
		return
	default:
		utils.Debugf("[SOCKS5] %s: unknown address type 0x%02x", remote, buf[3])
		clientConn.Write(socks5Reply(replyAddressNotSupported))
		return
	}

	utils.Debugf("[SOCKS5] %s: CONNECT %s", remote, targetAddr)

	targetConn, err := s.dialer.DialTCP(targetAddr)
	if err != nil {
		utils.Debugf("[SOCKS5] %s: dial %s failed: %v", remote, targetAddr, err)
		clientConn.Write(socks5Reply(replyHostUnreachable))
		return
	}
	defer targetConn.Close()

	clientConn.Write(socks5Reply(replySucceeded))

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		defer targetConn.Close()
		io.Copy(&countingWriter{targetConn, &s.bytesSent}, clientConn)
	}()

	go func() {
		defer wg.Done()
		defer clientConn.Close()
		io.Copy(&countingWriter{clientConn, &s.bytesReceived}, targetConn)
	}()

	wg.Wait()
}

func containsMethod(methods []byte, target byte) bool {
	for _, m := range methods {
		if m == target {
			return true
		}
	}
	return false
}

// authenticate performs RFC 1929 username/password subnegotiation. It writes
// the required reply either way, and returns whether the credentials
// matched (constant-time, to avoid leaking a timing signal on the
// comparison).
func (s *SOCKS5Server) authenticate(clientConn net.Conn) bool {
	buf := make([]byte, 513) // ver(1) + ulen(1) + uname(<=255) + plen(1) + passwd(<=255)
	n, err := clientConn.Read(buf)
	if err != nil || n < 2 || buf[0] != 0x01 {
		return false
	}
	ulen := int(buf[1])
	if 2+ulen+1 > n {
		return false
	}
	username := buf[2 : 2+ulen]
	plen := int(buf[2+ulen])
	if 3+ulen+plen > n {
		return false
	}
	password := buf[3+ulen : 3+ulen+plen]

	usernameOK := subtle.ConstantTimeCompare(username, []byte(s.username)) == 1
	passwordOK := subtle.ConstantTimeCompare(password, []byte(s.password)) == 1
	if usernameOK && passwordOK {
		clientConn.Write([]byte{0x01, 0x00})
		return true
	}
	clientConn.Write([]byte{0x01, 0x01})
	return false
}
