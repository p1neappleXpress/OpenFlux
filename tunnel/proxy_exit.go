package tunnel

import "openflux/transport"

type proxyExit struct {
	trans         transport.Transport
	tun           *TCPTunnel
	upstreamProxy string
}

func newProxyExit(trans transport.Transport, upstreamProxy ...string) *proxyExit {
	proxy := ""
	if len(upstreamProxy) > 0 {
		proxy = upstreamProxy[0]
	}
	return &proxyExit{trans: trans, upstreamProxy: proxy}
}

func (p *proxyExit) Mode() string { return "proxy" }

func (p *proxyExit) Start() error {
	p.tun = NewTCPTunnelModeWithProxy(p.trans, true, ExitModeL4, p.upstreamProxy)
	return nil
}

func (p *proxyExit) Stop() error { return nil }
