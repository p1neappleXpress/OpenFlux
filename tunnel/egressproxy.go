package tunnel

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/net/proxy"
)

// Egress proxy support (opffk fork): route the l4 exit's re-dial of
// client destinations through a SOCKS5 or HTTP CONNECT proxy instead of
// dialing them directly. l3 (raw-socket SNAT) has no proxy path and must
// not enable this — kernel-level NAT cannot chain into a SOCKS5 proxy.

// EgressProxyDialer dials one exited flow.
type EgressProxyDialer interface {
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}

// egressDialer is nil unless SetEgressProxy was called; nil means dial
// destinations directly (the classic behavior).
var egressDialer EgressProxyDialer

// SetEgressProxy routes the l4 exit's TCP re-dial through d.
func SetEgressProxy(d EgressProxyDialer) { egressDialer = d }

// egressDialTCP dials dest for an exiting TCP flow: through the egress
// proxy when one is set, directly otherwise. The timeout matches the
// direct dial the exit always used.
func egressDialTCP(dest string) (net.Conn, error) {
	if egressDialer == nil {
		return net.DialTimeout("tcp", dest, 10*time.Second)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return egressDialer.DialContext(ctx, "tcp", dest)
}

// egressDialUDP dials dest for an exiting UDP flow. A SOCKS5/HTTP proxy
// cannot carry it (SOCKS5 UDP ASSOCIATE is not implemented, HTTP has no
// datagram path at all), and dialing directly would silently bypass the
// proxy the operator chose — so the flow is refused instead.
func egressDialUDP(dest string) (net.Conn, error) {
	if egressDialer != nil {
		return nil, fmt.Errorf("egress proxy is set: UDP is refused (TCP only), direct dial would bypass the proxy")
	}
	return net.DialTimeout("udp", dest, 10*time.Second)
}

// NewEgressProxyDialer parses an --egress-proxy URL: socks5:// and
// http:// (CONNECT), optional user:pass@. https is not supported: the
// exit would have to verify the proxy's certificate, and the intended
// use is a plain proxy on a LAN or a self-hosted VPS.
func NewEgressProxyDialer(raw string) (EgressProxyDialer, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	switch u.Scheme {
	case "socks5", "socks5h", "http":
	default:
		return nil, fmt.Errorf("scheme %q: want socks5 or http", u.Scheme)
	}
	if u.Hostname() == "" || u.Port() == "" {
		return nil, fmt.Errorf("want host:port")
	}
	var auth *proxy.Auth
	if u.User != nil {
		pass, _ := u.User.Password()
		auth = &proxy.Auth{User: u.User.Username(), Password: pass}
	}
	addr := net.JoinHostPort(u.Hostname(), u.Port())

	switch u.Scheme {
	case "socks5", "socks5h":
		inner, err := proxy.SOCKS5("tcp", addr, auth, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("socks5 dialer: %w", err)
		}
		return socksDialer{inner: inner}, nil
	default:
		return &httpConnectDialer{addr: addr, auth: auth}, nil
	}
}

// socksDialer adapts x/net/proxy's dialer to EgressProxyDialer, running
// a plain Dial under the context when the library build has no
// DialContext.
type socksDialer struct {
	inner proxy.Dialer
}

func (s socksDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if cd, ok := s.inner.(proxy.ContextDialer); ok {
		return cd.DialContext(ctx, network, addr)
	}
	type result struct {
		conn net.Conn
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		c, e := s.inner.Dial(network, addr)
		ch <- result{c, e}
	}()
	select {
	case r := <-ch:
		return r.conn, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// httpConnectDialer tunnels through an HTTP proxy with CONNECT.
type httpConnectDialer struct {
	addr string
	auth *proxy.Auth
}

func (d *httpConnectDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if network != "tcp" {
		return nil, fmt.Errorf("egress proxy: network %q", network)
	}
	var dc net.Dialer
	conn, err := dc.DialContext(ctx, "tcp", d.addr)
	if err != nil {
		return nil, fmt.Errorf("connect to proxy: %w", err)
	}
	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Scheme: "http", Host: addr},
		Host:   addr,
		Header: make(http.Header),
	}
	if d.auth != nil {
		token := base64.StdEncoding.EncodeToString([]byte(d.auth.User + ":" + d.auth.Password))
		req.Header.Set("Proxy-Authorization", "Basic "+token)
	}
	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("send CONNECT: %w", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read CONNECT response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		conn.Close()
		return nil, fmt.Errorf("proxy answered CONNECT %s: %s", resp.Status, resp.Proto)
	}
	return conn, nil
}
