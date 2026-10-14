package tunnel

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DialFunc dials an outbound connection from the exit node.
// It mirrors net.Dial semantics: network is "tcp" (L4 exit) and
// address is a host:port join (IP literal, IPv6 in brackets, or DNS name).
type DialFunc func(network, address string) (net.Conn, error)

// DirectDial is the historical exit behavior: a plain TCP dial
// with a 10s timeout, no proxy involved.
func DirectDial(network, address string) (net.Conn, error) {
	return net.DialTimeout(network, address, 10*time.Second)
}

// UpstreamProxy is an HTTP CONNECT proxy the L4 exit dials through.
// Only plain HTTP (no TLS to the proxy) is supported in v1.
// Auth is optional (http://user:pass@host:port); without credentials
// no Proxy-Authorization header is sent.
type UpstreamProxy struct {
	// Addr is the proxy endpoint, always host:port.
	Addr string
	// Username/Password for Basic auth. Empty means no auth header.
	Username string
	Password string
	// DialTimeout for the TCP connect to the proxy.
	DialTimeout time.Duration
	// HandshakeTimeout for writing CONNECT and reading the response.
	HandshakeTimeout time.Duration
}

// ParseUpstreamProxy parses --upstream-proxy values:
//
//	127.0.0.1:10808
//	http://127.0.0.1:10808
//	http://user:pass@127.0.0.1:10808
//
// The scheme, when present, must be http. Anything else (socks5, https, ...)
// is rejected with a clear error. Empty string returns (nil, nil): no upstream.
func ParseUpstreamProxy(s string) (*UpstreamProxy, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	raw := s
	if !strings.Contains(s, "://") {
		raw = "http://" + s
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("upstream proxy: bad URL %q: %w", s, err)
	}
	if u.Scheme != "" && u.Scheme != "http" {
		return nil, fmt.Errorf("upstream proxy: unsupported scheme %q (want http://host:port)", u.Scheme)
	}
	host := u.Host
	if host == "" {
		return nil, fmt.Errorf("upstream proxy: missing host:port in %q", s)
	}
	if _, _, err := net.SplitHostPort(host); err != nil {
		return nil, fmt.Errorf("upstream proxy: bad host:port %q: %w", host, err)
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("upstream proxy: missing host in %q", s)
	}
	p := &UpstreamProxy{
		Addr:             host,
		DialTimeout:      10 * time.Second,
		HandshakeTimeout: 10 * time.Second,
	}
	if u.User != nil {
		p.Username = u.User.Username()
		p.Password, _ = u.User.Password()
	}
	return p, nil
}

// RedactedAddr returns host:port, safe for logs.
func (p *UpstreamProxy) RedactedAddr() string {
	if p == nil {
		return ""
	}
	return p.Addr
}

// StringRedacted is host:port plus a user hint, never the password.
func (p *UpstreamProxy) StringRedacted() string {
	if p == nil {
		return ""
	}
	if p.Username != "" {
		return fmt.Sprintf("http://%s@%s", p.Username, p.Addr)
	}
	return "http://" + p.Addr
}

// Dial implements DialFunc with background context.
func (p *UpstreamProxy) Dial(network, address string) (net.Conn, error) {
	return p.DialContext(context.Background(), network, address)
}

// DialContext dials the proxy, performs the CONNECT handshake for
// target address, and returns the tunneled connection on success.
// The returned conn first serves bytes already buffered while reading
// the proxy response, so no application bytes are lost.
func (p *UpstreamProxy) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if p == nil {
		return nil, fmt.Errorf("upstream proxy: nil proxy")
	}
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("upstream proxy: network %q not supported (want tcp)", network)
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("upstream proxy: bad target %q: %w", address, err)
	}
	if host == "" || port == "" {
		return nil, fmt.Errorf("upstream proxy: bad target %q", address)
	}
	// Normalize the target the same way net.JoinHostPort does, so IPv6
	// literals keep their brackets and domain names pass through untouched.
	target := net.JoinHostPort(host, port)

	dialTimeout := p.DialTimeout
	if dialTimeout <= 0 {
		dialTimeout = 10 * time.Second
	}
	handshakeTimeout := p.HandshakeTimeout
	if handshakeTimeout <= 0 {
		handshakeTimeout = 10 * time.Second
	}

	d := net.Dialer{Timeout: dialTimeout}
	conn, err := d.DialContext(ctx, "tcp", p.Addr)
	if err != nil {
		return nil, fmt.Errorf("upstream proxy %s: dial: %w", p.RedactedAddr(), err)
	}
	// From here on every failure path must close conn: no leaks.
	success := false
	defer func() {
		if !success {
			_ = conn.Close()
		}
	}()

	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}

	deadline := time.Now().Add(handshakeTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("upstream proxy %s: set deadline: %w", p.RedactedAddr(), err)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n", target, target)
	if p.Username != "" {
		cred := p.Username + ":" + p.Password
		fmt.Fprintf(&sb, "Proxy-Authorization: Basic %s\r\n", base64.StdEncoding.EncodeToString([]byte(cred)))
	}
	sb.WriteString("Proxy-Connection: Keep-Alive\r\n\r\n")
	if _, err := conn.Write([]byte(sb.String())); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("upstream proxy %s: write CONNECT: %w (context: %v)", p.RedactedAddr(), err, ctx.Err())
		}
		return nil, fmt.Errorf("upstream proxy %s: write CONNECT: %w", p.RedactedAddr(), err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("upstream proxy %s: read CONNECT response: %w (context: %v)", p.RedactedAddr(), err, ctx.Err())
		}
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			return nil, fmt.Errorf("upstream proxy %s: CONNECT timeout: %w", p.RedactedAddr(), err)
		}
		return nil, fmt.Errorf("upstream proxy %s: bad CONNECT response: %w", p.RedactedAddr(), err)
	}
	// CONNECT responses carry no body; drain/close the body without
	// touching the underlying conn (http.NoBody for 1xx/204/304 and
	// an empty body here).
	if resp.Body != nil {
		_ = resp.Body.Close()
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream proxy %s: CONNECT %s -> %s", p.RedactedAddr(), target, resp.Status)
	}

	// Handshake done: back to blocking mode. The exit's own copy loops
	// manage their own timeouts from here.
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("upstream proxy %s: clear deadline: %w", p.RedactedAddr(), err)
	}

	success = true
	if br.Buffered() > 0 {
		return &bufferedConn{Conn: conn, r: br}, nil
	}
	return conn, nil
}

// bufferedConn replays bytes a bufio.Reader already pulled from the wire
// before handing reads to the underlying connection. http.ReadResponse
// may buffer tunneled bytes sent right after the proxy's headers; without
// this wrapper those bytes would be lost.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) {
	return c.r.Read(b)
}
