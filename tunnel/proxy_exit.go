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
	tun, err := NewTCPTunnelMode(p.trans, true, ExitModeL4)
	if err != nil {
		return err
	}
	p.tun = tun
	return nil
}

func (p *proxyExit) Stop() error { return nil }
