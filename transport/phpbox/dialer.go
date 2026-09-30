package phpbox

import (
	"context"
	"net"
	"strconv"
)

// SocksDialer adapts a Mux to the socks5.Dialer interface (DialTCP), so a
// local SOCKS5 server can send every app connection out as a mux stream.
// It is defined structurally (no socks5 import) to avoid a dependency edge.
type SocksDialer struct {
	m *Mux
}

// NewSocksDialer wraps m. Each DialTCP opens one stream to the requested
// address through the exit.
func NewSocksDialer(m *Mux) *SocksDialer { return &SocksDialer{m: m} }

// DialTCP opens a stream to address ("host:port") through the exit.
func (d *SocksDialer) DialTCP(address string) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, err
	}
	return d.m.Dial(context.Background(), host, port)
}
