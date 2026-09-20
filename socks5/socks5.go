package socks5

import (
	"fmt"
	"io"
	"net"
	"sync"

	"openflux/utils"
)

type Dialer interface {
	DialTCP(address string) (net.Conn, error)
}

type SOCKS5Server struct {
	listenAddr string
	dialer     Dialer

	mu       sync.Mutex
	listener net.Listener
	closed   bool
}

func NewSOCKS5Server(addr string, dialer Dialer) *SOCKS5Server {
	return &SOCKS5Server{listenAddr: addr, dialer: dialer}
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

// sendReply writes a SOCKS5 reply with the given REP code and a zeroed
// IPv4 BND.ADDR/BND.PORT, as required by RFC 1928 section 6.
func sendReply(conn net.Conn, rep byte) {
	conn.Write([]byte{0x05, rep, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})
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

	// Greeting: VER NMETHODS METHODS. Fields are read with io.ReadFull to
	// their exact RFC 1928 lengths: a single Read may return a fragmented
	// message, and TCP makes no guarantees about message boundaries.
	head := make([]byte, 2)
	if _, err := io.ReadFull(clientConn, head); err != nil || head[0] != 0x05 {
		return
	}
	if head[1] > 0 {
		methods := make([]byte, head[1])
		if _, err := io.ReadFull(clientConn, methods); err != nil {
			return
		}
	}
	if _, err := clientConn.Write([]byte{0x05, 0x00}); err != nil {
		return
	}

	// Request: VER CMD RSV ATYP DST.ADDR DST.PORT
	req := make([]byte, 4)
	if _, err := io.ReadFull(clientConn, req); err != nil || req[0] != 0x05 {
		return
	}
	if req[1] != 0x01 {
		sendReply(clientConn, 0x07) // command not supported
		return
	}

	var targetAddr string
	switch req[3] {
	case 0x01:
		addr := make([]byte, 6)
		if _, err := io.ReadFull(clientConn, addr); err != nil {
			return
		}
		targetAddr = fmt.Sprintf("%d.%d.%d.%d:%d",
			addr[0], addr[1], addr[2], addr[3],
			uint16(addr[4])<<8|uint16(addr[5]))
	case 0x03:
		lenBuf := make([]byte, 1)
		if _, err := io.ReadFull(clientConn, lenBuf); err != nil {
			return
		}
		domain := make([]byte, lenBuf[0])
		if len(domain) == 0 {
			return
		}
		if _, err := io.ReadFull(clientConn, domain); err != nil {
			return
		}
		port := make([]byte, 2)
		if _, err := io.ReadFull(clientConn, port); err != nil {
			return
		}
		targetAddr = fmt.Sprintf("%s:%d",
			string(domain),
			uint16(port[0])<<8|uint16(port[1]))
	case 0x04:
		addr := make([]byte, 18)
		if _, err := io.ReadFull(clientConn, addr); err != nil {
			return
		}
		targetAddr = fmt.Sprintf("[%s]:%d",
			net.IP(addr[:16]).String(),
			uint16(addr[16])<<8|uint16(addr[17]))
	default:
		sendReply(clientConn, 0x08) // address type not supported
		return
	}

	utils.Debugf("[SOCKS5] CONNECT %s", targetAddr)

	targetConn, err := s.dialer.DialTCP(targetAddr)
	if err != nil {
		utils.Debugf("[SOCKS5] Dial failed: %v", err)
		sendReply(clientConn, 0x04)
		return
	}
	defer targetConn.Close()

	if _, err := clientConn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}); err != nil {
		return
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		defer targetConn.Close()
		io.Copy(targetConn, clientConn)
	}()

	go func() {
		defer wg.Done()
		defer clientConn.Close()
		io.Copy(clientConn, targetConn)
	}()

	wg.Wait()
}
