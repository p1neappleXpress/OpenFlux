package tunnel

import "openflux/transport"

type proxyExit struct {
	trans transport.Transport
	tun   *TCPTunnel
}

func newProxyExit(trans transport.Transport) *proxyExit {
	return &proxyExit{trans: trans}
}

func (p *proxyExit) Mode() string { return "proxy" }

func (p *proxyExit) Start() error {
	p.tun = NewTCPTunnelMode(p.trans, true, ExitModeL4)
	return nil
}

func (p *proxyExit) Stop() error {
	if p.tun != nil {
		p.tun.Close()
	}
	return nil
}
