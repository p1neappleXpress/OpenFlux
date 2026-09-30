// Package streamproxy is the client side of the phpbox stream mode as a
// library: a carrier (cups.online, a Mail.ru or Yandex document) carries the
// phpbox stream mux to an exit on plain PHP hosting, and a local SOCKS5 (and
// optionally HTTP) proxy hands every app connection to a mux stream. It is one
// implementation for the CLI (--mode=stream), Android and iOS, so the modes
// behave the same everywhere. No gVisor and no IP packets: TCP streams only.
package streamproxy

import (
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/p1neappleXpress/OpenFlux/socks5"
	"github.com/p1neappleXpress/OpenFlux/transport/phpbox"
	"github.com/p1neappleXpress/OpenFlux/tunnel"
	"github.com/p1neappleXpress/OpenFlux/utils"
)

// Options configure Start.
type Options struct {
	// Carrier moves the mux's bytes (a transport.Transport is one as-is).
	Carrier phpbox.Carrier
	// Socks is the SOCKS5 listen address ("127.0.0.1:1080").
	Socks string
	// HTTP, if set, also serves an HTTP proxy (CONNECT and plain requests) there.
	HTTP string
	// Username and Password, if Username is set, are required by the SOCKS5 server.
	Username, Password string
	// Wrap, if set, wraps the dialer (split routing: some domains go direct).
	Wrap func(socks5.Dialer) socks5.Dialer
	// Label names the carrier in logs.
	Label string
}

// Proxy is a running stream-mode client.
type Proxy struct {
	mux    *phpbox.Mux
	socks  *socks5.SOCKS5Server
	httpLn net.Listener
	opts   Options

	mu      sync.Mutex
	stopped bool
}

// Start brings the carrier up, binds the listeners and serves in the
// background. It returns once the listeners are bound (the carrier may still be
// joining: ask Connected).
func Start(o Options) (*Proxy, error) {
	if o.Carrier == nil {
		return nil, errors.New("streamproxy: no carrier")
	}
	if o.Socks == "" {
		return nil, errors.New("streamproxy: no SOCKS5 address")
	}
	m := phpbox.NewMux(o.Carrier)
	if err := m.Start(); err != nil {
		return nil, fmt.Errorf("start carrier: %w", err)
	}
	var d socks5.Dialer = phpbox.NewSocksDialer(m)
	if o.Wrap != nil {
		d = o.Wrap(d)
	}
	srv := socks5.NewSOCKS5Server(o.Socks, d)
	if o.Username != "" {
		srv.SetAuth(o.Username, o.Password)
	}
	if err := srv.Bind(); err != nil {
		_ = m.Close()
		return nil, fmt.Errorf("bind %s: %w", o.Socks, err)
	}
	p := &Proxy{mux: m, socks: srv, opts: o}
	if o.HTTP != "" {
		ln, err := net.Listen("tcp", o.HTTP)
		if err != nil {
			_ = srv.Close()
			_ = m.Close()
			return nil, fmt.Errorf("bind %s: %w", o.HTTP, err)
		}
		p.httpLn = ln
		utils.SafeGo("streamproxy.http", func() { _ = tunnel.ServeHTTPProxy(ln, d.DialTCP) })
	}
	utils.SafeGo("streamproxy.socks", func() { _ = srv.Start() })
	return p, nil
}

// Stop ends the proxy and the carrier.
func (p *Proxy) Stop() {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return
	}
	p.stopped = true
	p.mu.Unlock()
	_ = p.socks.Close()
	if p.httpLn != nil {
		_ = p.httpLn.Close()
	}
	_ = p.mux.Close()
}

// Connected reports whether the carrier has joined its room or document (a
// carrier that cannot say counts as connected).
func (p *Proxy) Connected() bool {
	if c, ok := p.opts.Carrier.(interface{ IsConnected() bool }); ok {
		return c.IsConnected()
	}
	return true
}

// BytesSent and BytesReceived are running totals relayed through the SOCKS5
// server (app -> internet, internet -> app), for a speed indicator.
func (p *Proxy) BytesSent() int64     { return p.socks.BytesSent() }
func (p *Proxy) BytesReceived() int64 { return p.socks.BytesReceived() }

// Mux exposes the mux (Dial) for callers that run their own listeners.
func (p *Proxy) Mux() *phpbox.Mux { return p.mux }
