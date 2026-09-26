package socks5

import (
	"crypto/subtle"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"openflux/utils"
)

type Dialer interface {
	DialTCP(address string) (net.Conn, error)
}

// UDPDialer is optional, preserving compatibility with TCP-only integrations.
type UDPDialer interface {
	DialUDP(address string) (net.Conn, error)
}

type SOCKS5Server struct {
	listenAddr string
	dialer     Dialer
	username   string
	password   string

	mu       sync.Mutex
	listener net.Listener
	closed   bool
	clients  map[net.Conn]struct{}

	bytesSent     atomic.Int64
	bytesReceived atomic.Int64
}

func NewSOCKS5Server(addr string, dialer Dialer) *SOCKS5Server {
	return &SOCKS5Server{listenAddr: addr, dialer: dialer, clients: make(map[net.Conn]struct{})}
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
	for conn := range s.clients {
		_ = conn.Close()
	}
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

func (s *SOCKS5Server) handleConnection(clientConn net.Conn) {
	s.mu.Lock()
	if s.closed || len(s.clients) >= 256 {
		s.mu.Unlock()
		_ = clientConn.Close()
		return
	}
	if s.clients == nil {
		s.clients = make(map[net.Conn]struct{})
	}
	s.clients[clientConn] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.clients, clientConn)
		s.mu.Unlock()
	}()
	_ = clientConn.SetDeadline(time.Now().Add(10 * time.Second))
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

	var greeting [2]byte
	if _, err := io.ReadFull(clientConn, greeting[:]); err != nil || greeting[0] != 0x05 {
		utils.Debugf("[SOCKS5] %s: bad greeting: %v", remote, err)
		return
	}
	methods := make([]byte, int(greeting[1]))
	if _, err := io.ReadFull(clientConn, methods); err != nil {
		utils.Debugf("[SOCKS5] %s: truncated method list: %v", remote, err)
		return
	}

	if s.username != "" {
		if !containsMethod(methods, 0x02) {
			utils.Debugf("[SOCKS5] %s: client didn't offer username/password auth, rejecting", remote)
			_, _ = clientConn.Write([]byte{0x05, 0xFF})
			return
		}
		if _, err := clientConn.Write([]byte{0x05, 0x02}); err != nil {
			return
		}
		if !s.authenticate(clientConn) {
			utils.Debugf("[SOCKS5] %s: authentication failed", remote)
			return
		}
		utils.Debugf("[SOCKS5] %s: authenticated", remote)
	} else if !containsMethod(methods, 0x00) {
		utils.Debugf("[SOCKS5] %s: client offered no acceptable auth method", remote)
		_, _ = clientConn.Write([]byte{0x05, 0xFF})
		return
	} else if _, err := clientConn.Write([]byte{0x05, 0x00}); err != nil {
		return
	}

	var request [4]byte
	if _, err := io.ReadFull(clientConn, request[:]); err != nil || request[0] != 0x05 || request[2] != 0 {
		utils.Debugf("[SOCKS5] %s: bad request: %v", remote, err)
		return
	}
	targetAddr, err := readAddress(clientConn, request[3])
	if err != nil {
		utils.Debugf("[SOCKS5] %s: bad target address: %v", remote, err)
		writeReply(clientConn, replyAddressNotSupported, nil)
		return
	}

	_ = clientConn.SetDeadline(time.Time{})
	switch request[1] {
	case 0x01:
		s.handleConnect(clientConn, remote, targetAddr)
	case 0x03:
		s.handleUDPAssociate(clientConn, targetAddr)
	default:
		utils.Debugf("[SOCKS5] %s: unsupported command 0x%02x", remote, request[1])
		writeReply(clientConn, replyCommandNotSupported, nil)
	}
}

func (s *SOCKS5Server) handleConnect(clientConn net.Conn, remote net.Addr, targetAddr string) {
	utils.Debugf("[SOCKS5] %s: CONNECT %s", remote, targetAddr)

	targetConn, err := s.dialer.DialTCP(targetAddr)
	if err != nil {
		utils.Debugf("[SOCKS5] %s: dial %s failed: %v", remote, targetAddr, err)
		writeReply(clientConn, replyHostUnreachable, nil)
		return
	}
	defer targetConn.Close()

	if err := writeReply(clientConn, replySucceeded, targetConn.LocalAddr()); err != nil {
		return
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		defer targetConn.Close()
		n, _ := io.Copy(&countingWriter{targetConn, &s.bytesSent}, clientConn)
		utils.Debugf("[SOCKS5] %s: -> %s sent %d bytes", remote, targetAddr, n)
	}()

	go func() {
		defer wg.Done()
		defer clientConn.Close()
		n, _ := io.Copy(&countingWriter{clientConn, &s.bytesReceived}, targetConn)
		utils.Debugf("[SOCKS5] %s: <- %s received %d bytes", remote, targetAddr, n)
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
	// ver(1) ulen(1) uname(ulen) plen(1) passwd(plen); read field by field,
	// since one Read may return only part of the request.
	var head [2]byte
	if _, err := io.ReadFull(clientConn, head[:]); err != nil || head[0] != 0x01 {
		return false
	}
	username := make([]byte, int(head[1]))
	if _, err := io.ReadFull(clientConn, username); err != nil {
		return false
	}
	var plen [1]byte
	if _, err := io.ReadFull(clientConn, plen[:]); err != nil {
		return false
	}
	password := make([]byte, int(plen[0]))
	if _, err := io.ReadFull(clientConn, password); err != nil {
		return false
	}

	usernameOK := subtle.ConstantTimeCompare(username, []byte(s.username)) == 1
	passwordOK := subtle.ConstantTimeCompare(password, []byte(s.password)) == 1
	if usernameOK && passwordOK {
		clientConn.Write([]byte{0x01, 0x00})
		return true
	}
	clientConn.Write([]byte{0x01, 0x01})
	return false
}

func (s *SOCKS5Server) handleUDPAssociate(control net.Conn, requestedAddr string) {
	dialer, ok := s.dialer.(UDPDialer)
	if !ok {
		_ = writeReply(control, 0x07, nil)
		return
	}
	requestedHost, requestedService, err := net.SplitHostPort(requestedAddr)
	if err != nil {
		_ = writeReply(control, 0x08, nil)
		return
	}
	requestedIP := net.ParseIP(requestedHost)
	var requestedPort int
	_, _ = fmt.Sscanf(requestedService, "%d", &requestedPort)
	// Do not resolve the association's source address using local DNS.
	if requestedIP == nil {
		_ = writeReply(control, 0x08, nil)
		return
	}
	bindIP := net.ParseIP("127.0.0.1")
	if host, _, err := net.SplitHostPort(control.LocalAddr().String()); err == nil {
		if parsed := net.ParseIP(host); parsed != nil {
			bindIP = parsed
		}
	}
	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: bindIP})
	if err != nil {
		writeReply(control, 0x01, nil)
		return
	}
	defer udpConn.Close()

	type udpFlow struct{ conn net.Conn }
	flows := make(map[string]udpFlow)
	var flowsMu sync.Mutex
	var closing bool
	var clientAddr *net.UDPAddr
	var clientMu sync.RWMutex
	expectedIP := net.ParseIP("127.0.0.1")
	if host, _, err := net.SplitHostPort(control.RemoteAddr().String()); err == nil {
		expectedIP = net.ParseIP(host)
	}
	if expectedIP == nil || (!requestedIP.IsUnspecified() && !requestedIP.Equal(expectedIP)) {
		_ = writeReply(control, 0x02, nil)
		return
	}
	if err := writeReply(control, 0x00, udpConn.LocalAddr()); err != nil {
		return
	}
	defer func() {
		_ = udpConn.Close()
		flowsMu.Lock()
		defer flowsMu.Unlock()
		closing = true
		for _, flow := range flows {
			_ = flow.conn.Close()
		}
	}()

	utils.SafeGo("socks5.udp", func() {
		buf := make([]byte, 65535)
		for {
			n, from, err := udpConn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if !from.IP.Equal(expectedIP) || (requestedPort != 0 && from.Port != requestedPort) {
				continue
			}
			clientMu.RLock()
			pinnedClient := clientAddr
			clientMu.RUnlock()
			if pinnedClient != nil &&
				(!from.IP.Equal(pinnedClient.IP) || from.Port != pinnedClient.Port) {
				continue
			}
			dest, payload, err := parseUDPRequest(buf[:n])
			if err != nil {
				continue
			}
			clientMu.Lock()
			clientAddr = from
			clientMu.Unlock()

			flowsMu.Lock()
			if closing {
				flowsMu.Unlock()
				return
			}
			flow, ok := flows[dest]
			if !ok {
				if len(flows) >= 256 {
					flowsMu.Unlock()
					continue
				}
				// Dial outside the lock so shutdown can close existing flows.
				flowsMu.Unlock()
				conn, err := dialer.DialUDP(dest)
				flowsMu.Lock()
				if err != nil {
					flowsMu.Unlock()
					continue
				}
				if closing {
					flowsMu.Unlock()
					_ = conn.Close()
					return
				}
				flow = udpFlow{conn: conn}
				flows[dest] = flow
				_ = conn.SetReadDeadline(time.Now().Add(2 * time.Minute))
				flowKey := dest
				utils.SafeGo("socks5.udp-response", func() {
					defer func() {
						_ = conn.Close()
						flowsMu.Lock()
						if current, ok := flows[flowKey]; ok && current.conn == conn {
							delete(flows, flowKey)
						}
						flowsMu.Unlock()
					}()
					response := make([]byte, 65535)
					for {
						n, err := conn.Read(response)
						if err != nil {
							return
						}
						_ = conn.SetReadDeadline(time.Now().Add(2 * time.Minute))
						packet := makeUDPResponse(conn.RemoteAddr(), response[:n])
						clientMu.RLock()
						to := clientAddr
						clientMu.RUnlock()
						if to != nil {
							_ = udpConn.SetWriteDeadline(time.Now().Add(10 * time.Second))
							_, _ = udpConn.WriteToUDP(packet, to)
						}
					}
				})
			}
			flowsMu.Unlock()
			_ = flow.conn.SetReadDeadline(time.Now().Add(2 * time.Minute))
			_ = flow.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := flow.conn.Write(payload); err != nil {
				_ = flow.conn.Close()
				flowsMu.Lock()
				if current, ok := flows[dest]; ok && current.conn == flow.conn {
					delete(flows, dest)
				}
				flowsMu.Unlock()
			}
		}
	})

	_, _ = io.Copy(io.Discard, control)
}

func readAddress(r io.Reader, atyp byte) (string, error) {
	var host string
	switch atyp {
	case 0x01:
		buf := make([]byte, 4)
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", err
		}
		host = net.IP(buf).String()
	case 0x03:
		var size [1]byte
		if _, err := io.ReadFull(r, size[:]); err != nil || size[0] == 0 {
			return "", fmt.Errorf("invalid domain length")
		}
		buf := make([]byte, int(size[0]))
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", err
		}
		host = string(buf)
	case 0x04:
		buf := make([]byte, 16)
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", err
		}
		host = net.IP(buf).String()
	default:
		return "", fmt.Errorf("unsupported address type %d", atyp)
	}
	var port [2]byte
	if _, err := io.ReadFull(r, port[:]); err != nil {
		return "", err
	}
	return net.JoinHostPort(host, fmt.Sprintf("%d", binary.BigEndian.Uint16(port[:]))), nil
}

func writeReply(w io.Writer, code byte, addr net.Addr) error {
	reply := append([]byte{0x05, code, 0x00}, encodeAddress(addr)...)
	_, err := w.Write(reply)
	return err
}

func addressString(addr net.Addr) string {
	if addr == nil {
		return "0.0.0.0:0"
	}
	return addr.String()
}

func parseUDPRequest(packet []byte) (string, []byte, error) {
	if len(packet) < 4 || packet[0] != 0 || packet[1] != 0 || packet[2] != 0 {
		return "", nil, fmt.Errorf("invalid or fragmented SOCKS5 UDP packet")
	}
	r := &sliceReader{data: packet[4:]}
	addr, err := readAddress(r, packet[3])
	if err != nil {
		return "", nil, err
	}
	return addr, r.data, nil
}

type sliceReader struct{ data []byte }

func (r *sliceReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func makeUDPResponse(addr net.Addr, payload []byte) []byte {
	out := append([]byte{0, 0, 0}, encodeAddress(addr)...)
	return append(out, payload...)
}

func encodeAddress(addr net.Addr) []byte {
	ip := net.IPv4zero.To4()
	atyp := byte(0x01)
	port := 0
	if host, service, err := net.SplitHostPort(addressString(addr)); err == nil {
		if parsed := net.ParseIP(host); parsed != nil {
			if v4 := parsed.To4(); v4 != nil {
				ip = v4
			} else {
				ip, atyp = parsed.To16(), 0x04
			}
		}
		fmt.Sscanf(service, "%d", &port)
	}
	out := append([]byte{atyp}, ip...)
	return append(out, byte(port>>8), byte(port))
}
