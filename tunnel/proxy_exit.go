package tunnel

import "github.com/p1neappleXpress/OpenFlux/transport"

type proxyExit struct {
	trans    transport.Transport
	tun      *TCPTunnel
	upstream *UpstreamProxy
}

func newProxyExit(trans transport.Transport) *proxyExit {
	return &proxyExit{trans: trans}
}

func newProxyExitWithUpstream(trans transport.Transport, upstream *UpstreamProxy) *proxyExit {
	return &proxyExit{trans: trans, upstream: upstream}
}

func (p *proxyExit) Mode() string { return "proxy" }

func (p *proxyExit) Start() error {
	p.tun = NewTCPTunnelMode(p.trans, true, ExitModeL4)
	if p.upstream != nil {
		p.tun.SetExitUpstream(p.upstream)
	}
	return nil
}

func (p *proxyExit) Stop() error {
	if p.tun != nil {
		p.tun.Close()
	}
	return nil
}
