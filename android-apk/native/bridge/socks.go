package bridge

import (
	"context"
	"net"
	"time"

	"golang.org/x/net/proxy"
)

type localSOCKS struct{ address string }

// x/net's SOCKS Conn embeds net.Conn and hides TCP CloseWrite. Keep the
// underlying socket to propagate an application half-close through SOCKS.
type socketCapture struct {
	net.Dialer
	conn *net.TCPConn
}

func (d *socketCapture) Dial(network, address string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, address)
}

func (d *socketCapture) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	c, err := d.Dialer.DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	d.conn = c.(*net.TCPConn)
	return c, nil
}

type halfCloseConn struct {
	net.Conn
	socket *net.TCPConn
}

func (c *halfCloseConn) CloseWrite() error { return c.socket.CloseWrite() }

func (d *localSOCKS) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	forward := &socketCapture{Dialer: net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}}
	dialer, err := proxy.SOCKS5("tcp", d.address, nil, forward)
	if err != nil {
		return nil, err
	}
	c, err := dialer.(proxy.ContextDialer).DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	return &halfCloseConn{Conn: c, socket: forward.conn}, nil
}
