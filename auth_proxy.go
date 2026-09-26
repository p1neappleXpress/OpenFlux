package main

import (
	"net"
	"sync"

	"openflux/transport"
	"openflux/tunnel"
	"openflux/utils"
)

// Local TCP ports reserved for the remote-auth side stack. They sit below
// gVisor's ephemeral range (16000+) and the usual OS ephemeral ranges, so
// they never collide with the main client path sharing the tunnel address.
const (
	authProxyPortLo = 12000
	authProxyPortHi = 12999
)

// remoteAuthProxy lazily starts the HTTP proxy the app uses to pass a check
// the exit reported (AuthRequired) from the exit's own address: a side TCP
// stack on the tunnel plus an HTTP proxy on loopback in front of it.
type remoteAuthProxy struct {
	demux *transport.PortDemux

	once sync.Once
	addr string
	err  error
}

func (p *remoteAuthProxy) Addr() (string, error) {
	p.once.Do(func() {
		side, err := tunnel.NewSideTunnel(p.demux.Side(), authProxyPortLo, authProxyPortHi)
		if err != nil {
			p.err = err
			return
		}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			side.Close()
			p.err = err
			return
		}
		p.addr = ln.Addr().String()
		utils.SafeGo("remote-auth-proxy", func() {
			_ = tunnel.ServeHTTPProxy(ln, side.DialTCP)
		})
		utils.Debugf("[AUTH] remote auth proxy on %s", p.addr)
	})
	return p.addr, p.err
}
