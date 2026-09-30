package mobile

import (
	"net"
	"sync"

	"github.com/p1neappleXpress/OpenFlux/transport"
	"github.com/p1neappleXpress/OpenFlux/tunnel"
	"github.com/p1neappleXpress/OpenFlux/utils"
)

// Local TCP ports of the side stack that carries the exit's captcha page.
// The exit answers only the client address, so the side stack shares it
// and is told apart by these ports: below gVisor's ephemeral range (16000+)
// and Android's (32768+), so the regular traffic never uses them.
const (
	authProxyPortLo = 12000
	authProxyPortHi = 12999
)

// authProxy is a loopback HTTP proxy whose connections leave through the
// tunnel and the exit, so a WebView pointed at it passes the exit's checks
// from the exit's address. Started on the first check the exit reports.
type authProxy struct {
	demux *transport.PortDemux

	mu     sync.Mutex
	addr   string
	ln     net.Listener
	side   *tunnel.TCPTunnel
	closed bool
}

func (p *authProxy) Addr() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.addr != "" || p.closed {
		return p.addr, nil
	}
	side, err := tunnel.NewSideTunnel(p.demux.Side(), authProxyPortLo, authProxyPortHi)
	if err != nil {
		return "", err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		side.Close()
		return "", err
	}
	p.side, p.ln, p.addr = side, ln, ln.Addr().String()
	utils.SafeGo("mobile.authProxy", func() { _ = tunnel.ServeHTTPProxy(ln, side.DialTCP) })
	return p.addr, nil
}

func (p *authProxy) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	if p.ln != nil {
		_ = p.ln.Close()
	}
	if p.side != nil {
		p.side.Close()
	}
	p.addr = ""
}

var currentAuthProxy struct {
	mu sync.Mutex
	p  *authProxy
}

// setAuthProxy makes p the proxy of the running Session (nil: none),
// closing the previous one.
func setAuthProxy(p *authProxy) {
	currentAuthProxy.mu.Lock()
	old := currentAuthProxy.p
	currentAuthProxy.p = p
	currentAuthProxy.mu.Unlock()
	if old != nil {
		old.Close()
	}
}
