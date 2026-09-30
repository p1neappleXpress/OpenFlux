package mobile

import (
	"fmt"
	"net"
	"strings"

	"github.com/p1neappleXpress/OpenFlux/socks5"
)

// splitDialer wraps another socks5.Dialer (the encrypted TCPTunnel) and
// dials matched domains directly from the device instead, for the Android
// "Маршрутизация" split-routing presets/custom domains. Domains are
// suffix-matched (a bare "youtube.com" also matches "www.youtube.com").
type splitDialer struct {
	tunnel socks5.Dialer
	bypass map[string]struct{}
}

func newSplitDialer(inner socks5.Dialer, bypassDomains []string) *splitDialer {
	set := make(map[string]struct{}, len(bypassDomains))
	for _, d := range bypassDomains {
		d = strings.ToLower(strings.TrimSpace(d))
		if d != "" {
			set[d] = struct{}{}
		}
	}
	return &splitDialer{tunnel: inner, bypass: set}
}

func (d *splitDialer) DialTCP(address string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(address)
	if err == nil && d.matches(host) {
		return net.Dial("tcp", address)
	}
	return d.tunnel.DialTCP(address)
}

// DialUDP keeps SOCKS5 UDP ASSOCIATE available behind the split dialer
// (the server only offers it when the dialer can dial UDP), with the same
// direct-or-tunnel choice as TCP.
func (d *splitDialer) DialUDP(address string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(address)
	if err == nil && d.matches(host) {
		return net.Dial("udp", address)
	}
	udp, ok := d.tunnel.(socks5.UDPDialer)
	if !ok {
		return nil, fmt.Errorf("tunnel dialer does not support UDP")
	}
	return udp.DialUDP(address)
}

func (d *splitDialer) matches(host string) bool {
	host = strings.ToLower(host)
	for domain := range d.bypass {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}
