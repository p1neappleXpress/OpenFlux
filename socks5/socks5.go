package socks5

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"universal-bypass-tool/utils"
)

const (
	socksVersion = 0x05

	methodNoAuth            = 0x00
	methodNotAccepted       = 0xff
	commandConnect          = 0x01
	addressTypeIPv4         = 0x01
	addressTypeDomain       = 0x03
	addressTypeIPv6         = 0x04
	replySucceeded          = 0x00
	replyGeneralFailure     = 0x01
	replyNotAllowed         = 0x02
	replyHostUnreachable    = 0x04
	replyConnectionRefused  = 0x05
	replyCommandUnsupported = 0x07
	replyAddressUnsupported = 0x08

	clientHandshakeTimeout = 10 * time.Second
)

type Dialer interface {
	DialTCP(address string) (net.Conn, error)
}

type SOCKS5Server struct {
	listenAddr string
	dialer     Dialer

	mu       sync.Mutex
	listener net.Listener
	active   map[net.Conn]struct{}
	closed   bool
	serving  bool
	clients  sync.WaitGroup
}

func NewSOCKS5Server(addr string, dialer Dialer) *SOCKS5Server {
	return &SOCKS5Server{
		listenAddr: addr,
		dialer:     dialer,
		active:     make(map[net.Conn]struct{}),
	}
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
	return s.Serve(listener)
}

// Serve accepts SOCKS5 clients from an already-bound listener. The listener is
// closed when Serve returns.
func (s *SOCKS5Server) Serve(listener net.Listener) error {
	if listener == nil {
		return errors.New("SOCKS5 listener is nil")
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = listener.Close()
		return net.ErrClosed
	}
	if s.serving {
		s.mu.Unlock()
		_ = listener.Close()
		return errors.New("SOCKS5 server is already serving")
	}
	if s.listener != nil && s.listener != listener {
		s.mu.Unlock()
		_ = listener.Close()
		return errors.New("SOCKS5 server already has a different listener")
	}
	s.listener = listener
	s.serving = true
	s.mu.Unlock()

	defer func() {
		_ = listener.Close()
		s.mu.Lock()
		s.serving = false
		s.mu.Unlock()
	}()
	utils.Debugf("[SOCKS5] Listening on %s", listener.Addr())

	var retryDelay time.Duration
	for {
		conn, err := listener.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed || errors.Is(err, net.ErrClosed) {
				utils.Debugf("[SOCKS5] Listener closed, stopping")
				return nil
			}
			if temporary, ok := err.(net.Error); ok && temporary.Temporary() {
				if retryDelay == 0 {
					retryDelay = 5 * time.Millisecond
				} else {
					retryDelay *= 2
				}
				if retryDelay > time.Second {
					retryDelay = time.Second
				}
				utils.Debugf("[SOCKS5] Accept error: %v; retrying in %s", err, retryDelay)
				time.Sleep(retryDelay)
				continue
			}
			return err
		}
		retryDelay = 0
		if !s.register(conn) {
			_ = conn.Close()
			continue
		}
		go func() {
			defer s.unregister(conn)
			s.handleConnection(conn)
		}()
	}
}

// Close stops the listener and all active SOCKS5 connections.
func (s *SOCKS5Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	listener := s.listener
	active := make([]net.Conn, 0, len(s.active))
	for conn := range s.active {
		active = append(active, conn)
	}
	s.mu.Unlock()

	var closeErr error
	if listener != nil {
		closeErr = listener.Close()
		if errors.Is(closeErr, net.ErrClosed) {
			closeErr = nil
		}
	}
	for _, conn := range active {
		_ = conn.Close()
	}
	s.clients.Wait()
	return closeErr
}

func (s *SOCKS5Server) register(conn net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.active[conn] = struct{}{}
	s.clients.Add(1)
	return true
}

func (s *SOCKS5Server) unregister(conn net.Conn) {
	s.mu.Lock()
	if _, exists := s.active[conn]; exists {
		delete(s.active, conn)
		s.clients.Done()
	}
	s.mu.Unlock()
}

func (s *SOCKS5Server) handleConnection(clientConn net.Conn) {
	defer func() {
		if recovered := recover(); recovered != nil {
			utils.Debugf("[SOCKS5] Recovered from panic in handler: %v", recovered)
		}
	}()
	defer clientConn.Close()

	_ = clientConn.SetDeadline(time.Now().Add(clientHandshakeTimeout))
	if err := negotiateMethod(clientConn); err != nil {
		return
	}
	targetAddr, replyCode, err := readConnectRequest(clientConn)
	if err != nil {
		if replyCode != 0 {
			_ = writeReply(clientConn, replyCode)
		}
		return
	}
	_ = clientConn.SetDeadline(time.Time{})

	utils.Debugf("[SOCKS5] CONNECT %s", targetAddr)
	if s.dialer == nil {
		_ = writeReply(clientConn, replyGeneralFailure)
		return
	}
	targetConn, err := s.dialer.DialTCP(targetAddr)
	if err != nil {
		utils.Debugf("[SOCKS5] Dial failed: %v", err)
		_ = writeReply(clientConn, replyCodeForDialError(err))
		return
	}
	defer targetConn.Close()

	if err := writeReply(clientConn, replySucceeded); err != nil {
		return
	}
	relayConnections(clientConn, targetConn)
}

func negotiateMethod(conn net.Conn) error {
	header := make([]byte, 2)
	if _, err := io.ReadFull(conn, header); err != nil {
		return err
	}
	if header[0] != socksVersion {
		return errors.New("unsupported SOCKS version")
	}
	methodCount := int(header[1])
	if methodCount == 0 {
		_ = writeFull(conn, []byte{socksVersion, methodNotAccepted})
		return errors.New("SOCKS client offered no methods")
	}
	methods := make([]byte, methodCount)
	if _, err := io.ReadFull(conn, methods); err != nil {
		return err
	}
	for _, method := range methods {
		if method == methodNoAuth {
			return writeFull(conn, []byte{socksVersion, methodNoAuth})
		}
	}
	_ = writeFull(conn, []byte{socksVersion, methodNotAccepted})
	return errors.New("SOCKS client did not offer no-auth")
}

func readConnectRequest(conn net.Conn) (string, byte, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return "", 0, err
	}
	if header[0] != socksVersion || header[2] != 0 {
		return "", replyGeneralFailure, errors.New("invalid SOCKS request header")
	}
	if header[1] != commandConnect {
		return "", replyCommandUnsupported, errors.New("unsupported SOCKS command")
	}

	var host string
	switch header[3] {
	case addressTypeIPv4:
		address := make([]byte, net.IPv4len)
		if _, err := io.ReadFull(conn, address); err != nil {
			return "", 0, err
		}
		host = net.IP(address).String()
	case addressTypeDomain:
		length := []byte{0}
		if _, err := io.ReadFull(conn, length); err != nil {
			return "", 0, err
		}
		if length[0] == 0 {
			return "", replyAddressUnsupported, errors.New("empty SOCKS domain")
		}
		domain := make([]byte, int(length[0]))
		if _, err := io.ReadFull(conn, domain); err != nil {
			return "", 0, err
		}
		host = string(domain)
	case addressTypeIPv6:
		return "", replyAddressUnsupported, errors.New("IPv6 is not supported")
	default:
		return "", replyAddressUnsupported, errors.New("unsupported SOCKS address type")
	}

	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(conn, portBytes); err != nil {
		return "", 0, err
	}
	port := binary.BigEndian.Uint16(portBytes)
	if port == 0 {
		return "", replyGeneralFailure, errors.New("invalid SOCKS destination port")
	}
	return net.JoinHostPort(host, strconv.Itoa(int(port))), 0, nil
}

func replyCodeForDialError(err error) byte {
	var coded interface{ SOCKS5ReplyCode() byte }
	if errors.As(err, &coded) {
		code := coded.SOCKS5ReplyCode()
		if code >= replyGeneralFailure && code <= replyAddressUnsupported {
			return code
		}
	}
	return replyHostUnreachable
}

func writeReply(conn net.Conn, code byte) error {
	return writeFull(conn, []byte{socksVersion, code, 0, addressTypeIPv4, 0, 0, 0, 0, 0, 0})
}

func writeFull(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}

func relayConnections(left, right net.Conn) {
	var closeOnce sync.Once
	closeBoth := func() {
		_ = left.Close()
		_ = right.Close()
	}
	done := make(chan struct{}, 2)
	copyOneWay := func(destination, source net.Conn) {
		_, _ = io.Copy(destination, source)
		closeOnce.Do(closeBoth)
		done <- struct{}{}
	}
	go copyOneWay(left, right)
	go copyOneWay(right, left)
	<-done
	<-done
}
