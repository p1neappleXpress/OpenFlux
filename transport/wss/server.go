package wss

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type ServerConfig struct {
	ListenAddr       string
	CertFile         string
	KeyFile          string
	AuthToken        string
	DenyCIDRs        []string
	DialTimeout      time.Duration
	HandshakeTimeout time.Duration
	RequestTimeout   time.Duration
}

type dialContextFunc func(context.Context, string, string) (net.Conn, error)

// Server is an authenticated TLS WebSocket exit. It tracks hijacked
// connections explicitly because net/http Shutdown does not own them.
type Server struct {
	config      ServerConfig
	certificate tls.Certificate
	policy      *DestinationPolicy
	dialContext dialContextFunc
	httpServer  *http.Server

	mu           sync.Mutex
	listener     net.Listener
	active       map[*websocket.Conn]struct{}
	shuttingDown bool
	serving      bool
	connections  sync.WaitGroup
}

func NewServer(config ServerConfig) (*Server, error) {
	if err := validateClientToken(config.AuthToken); err != nil {
		return nil, err
	}
	certificate, err := tls.LoadX509KeyPair(config.CertFile, config.KeyFile)
	if err != nil {
		return nil, err
	}
	policy, err := NewDestinationPolicy(config.DenyCIDRs)
	if err != nil {
		return nil, err
	}
	if config.DialTimeout <= 0 {
		config.DialTimeout = 10 * time.Second
	}
	if config.HandshakeTimeout <= 0 {
		config.HandshakeTimeout = 10 * time.Second
	}
	if config.RequestTimeout <= 0 {
		config.RequestTimeout = 10 * time.Second
	}

	netDialer := &net.Dialer{Timeout: config.DialTimeout, KeepAlive: 30 * time.Second}
	server := &Server{
		config:      config,
		certificate: certificate,
		policy:      policy,
		dialContext: netDialer.DialContext,
		active:      make(map[*websocket.Conn]struct{}),
	}
	server.httpServer = &http.Server{
		Handler:           http.HandlerFunc(server.handleTunnel),
		ReadHeaderTimeout: config.HandshakeTimeout,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    maxControlMessage,
		ErrorLog:          log.New(io.Discard, "", 0),
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{certificate},
			NextProtos:   []string{"http/1.1"},
		},
		TLSNextProto: make(map[string]func(*http.Server, *tls.Conn, http.Handler)),
	}
	return server, nil
}

// ListenAndServe binds the configured address and serves TLS until shutdown.
func (s *Server) ListenAndServe() error {
	listener, err := net.Listen("tcp", s.config.ListenAddr)
	if err != nil {
		return err
	}
	return s.Serve(listener)
}

// Serve accepts an already-bound listener. TLS is always applied by this
// method; it exists primarily to support deterministic ephemeral-port tests.
func (s *Server) Serve(listener net.Listener) error {
	if listener == nil {
		return errors.New("WSS listener is nil")
	}

	s.mu.Lock()
	if s.serving {
		s.mu.Unlock()
		_ = listener.Close()
		return errors.New("WSS server is already serving")
	}
	if s.shuttingDown {
		s.mu.Unlock()
		_ = listener.Close()
		return net.ErrClosed
	}
	s.serving = true
	s.listener = listener
	s.mu.Unlock()

	tlsListener := tls.NewListener(listener, s.httpServer.TLSConfig.Clone())
	err := s.httpServer.Serve(tlsListener)
	if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// Shutdown stops accepting new handshakes, closes active upgraded tunnels, and
// waits for their relay goroutines to finish or for ctx to expire.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if !s.shuttingDown {
		s.shuttingDown = true
	}
	active := make([]*websocket.Conn, 0, len(s.active))
	for conn := range s.active {
		active = append(active, conn)
	}
	s.mu.Unlock()

	for _, conn := range active {
		_ = conn.Close()
	}
	httpErr := s.httpServer.Shutdown(ctx)

	done := make(chan struct{})
	go func() {
		s.connections.Wait()
		close(done)
	}()
	select {
	case <-done:
		return httpErr
	case <-ctx.Done():
		if httpErr != nil {
			return errors.Join(httpErr, ctx.Err())
		}
		return ctx.Err()
	}
}

func (s *Server) handleTunnel(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Path != TunnelPath {
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	if request.URL.RawQuery != "" {
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		writer.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !bearerTokenMatches(request.Header.Get("Authorization"), s.config.AuthToken) {
		writer.Header().Set("WWW-Authenticate", "Bearer")
		writer.WriteHeader(http.StatusUnauthorized)
		return
	}
	if request.Header.Get("Origin") != "" {
		writer.WriteHeader(http.StatusForbidden)
		return
	}
	if !headerContainsToken(request.Header.Values("Sec-WebSocket-Protocol"), Subprotocol) {
		writer.WriteHeader(http.StatusBadRequest)
		return
	}

	upgrader := websocket.Upgrader{
		HandshakeTimeout:  s.config.HandshakeTimeout,
		ReadBufferSize:    32 * 1024,
		WriteBufferSize:   32 * 1024,
		Subprotocols:      []string{Subprotocol},
		EnableCompression: false,
		CheckOrigin: func(request *http.Request) bool {
			return request.Header.Get("Origin") == ""
		},
	}
	websocketConn, err := upgrader.Upgrade(writer, request, nil)
	if err != nil {
		return
	}
	if !s.register(websocketConn) {
		_ = websocketConn.Close()
		return
	}
	defer s.unregister(websocketConn)
	defer websocketConn.Close()

	websocketConn.SetReadLimit(maxControlMessage)
	_ = websocketConn.SetReadDeadline(time.Now().Add(s.config.RequestTimeout))
	var tunnelRequest connectRequest
	if err := readControl(websocketConn, &tunnelRequest); err != nil {
		s.respondError(websocketConn, responseBadRequest)
		return
	}
	if tunnelRequest.Version != ProtocolVersion {
		s.respondError(websocketConn, responseUnsupported)
		return
	}
	if tunnelRequest.Command != connectCommand || len(tunnelRequest.Target) == 0 || len(tunnelRequest.Target) > maxTargetLength {
		s.respondError(websocketConn, responseBadRequest)
		return
	}

	resolveContext, cancelResolve := context.WithTimeout(request.Context(), s.config.RequestTimeout)
	addresses, err := s.policy.Resolve(resolveContext, tunnelRequest.Target)
	cancelResolve()
	if err != nil {
		s.respondError(websocketConn, responseCodeForPolicyError(err))
		return
	}

	dialContext, cancelDial := context.WithTimeout(request.Context(), s.config.DialTimeout)
	var targetConn net.Conn
	for _, address := range addresses {
		targetConn, err = s.dialContext(dialContext, "tcp4", address)
		if err == nil {
			break
		}
	}
	cancelDial()
	if targetConn == nil {
		s.respondError(websocketConn, responseConnectFailed)
		return
	}
	defer targetConn.Close()

	_ = websocketConn.SetReadDeadline(time.Time{})
	_ = websocketConn.SetWriteDeadline(time.Now().Add(s.config.RequestTimeout))
	if err := writeControl(websocketConn, connectResponse{Version: ProtocolVersion, OK: true, Code: responseCodeOK}); err != nil {
		return
	}
	_ = websocketConn.SetWriteDeadline(time.Time{})
	websocketConn.SetReadLimit(maxFramePayload)

	relayConnections(newStreamConn(websocketConn), targetConn)
}

func (s *Server) respondError(conn *websocket.Conn, code string) {
	_ = conn.SetWriteDeadline(time.Now().Add(s.config.RequestTimeout))
	_ = writeControl(conn, connectResponse{Version: ProtocolVersion, OK: false, Code: code})
}

func (s *Server) register(conn *websocket.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.shuttingDown {
		return false
	}
	s.active[conn] = struct{}{}
	s.connections.Add(1)
	return true
}

func (s *Server) unregister(conn *websocket.Conn) {
	s.mu.Lock()
	if _, exists := s.active[conn]; exists {
		delete(s.active, conn)
		s.connections.Done()
	}
	s.mu.Unlock()
}

func bearerTokenMatches(header, expected string) bool {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return false
	}
	expectedDigest := sha256.Sum256([]byte(expected))
	actualDigest := sha256.Sum256([]byte(parts[1]))
	return subtle.ConstantTimeCompare(expectedDigest[:], actualDigest[:]) == 1
}

func headerContainsToken(values []string, expected string) bool {
	for _, value := range values {
		for _, token := range strings.Split(value, ",") {
			if strings.TrimSpace(token) == expected {
				return true
			}
		}
	}
	return false
}

func responseCodeForPolicyError(err error) string {
	switch {
	case errors.Is(err, ErrInvalidTarget):
		return responseBadRequest
	case errors.Is(err, ErrResolveFailed):
		return responseResolveFailed
	default:
		return responsePolicyDenied
	}
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
