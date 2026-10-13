package netbind

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"syscall"
	"time"
)

// Networks a carrier can be bound to (TransportConfig.Network). "" is the
// default route.
const (
	NetworkCellular = "cellular"
	NetworkWiFi     = "wifi"
	NetworkEthernet = "ethernet"
)

// NetworkBinder binds socket fd to a network before it connects: the
// platform's way (Android's Network.bindSocket, a Windows adapter).
type NetworkBinder func(network string, fd uintptr) error

var binder atomic.Pointer[NetworkBinder]

// SetNetworkBinder sets how sockets are bound to a network; nil clears it.
func SetNetworkBinder(f NetworkBinder) {
	if f == nil {
		binder.Store(nil)
		return
	}
	binder.Store(&f)
}

// WrapFor makes d follow the binding and then bind to network, so a
// bonded Session's carriers each leave through their own network (mobile
// data and Wi-Fi at once).
func WrapFor(network string, d *net.Dialer) *net.Dialer {
	d = Wrap(d)
	if network == "" {
		return d
	}
	own := d.Control
	d.Control = func(n, address string, c syscall.RawConn) error {
		if err := own(n, address, c); err != nil {
			return err
		}
		return bindNetwork(network, address, c)
	}
	return d
}

// DialerFor is a Dialer on network (30 s keepalive).
func DialerFor(network string, timeout time.Duration) *net.Dialer {
	return WrapFor(network, &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second})
}

// DialContextFor dials on network; "" is DialContext.
func DialContextFor(network string) func(ctx context.Context, n, address string) (net.Conn, error) {
	if network == "" {
		return DialContext
	}
	return func(ctx context.Context, n, address string) (net.Conn, error) {
		return DialerFor(network, 30*time.Second).DialContext(ctx, n, address)
	}
}

// HTTPTransport is an http.Client transport on network; nil (the default
// transport) for "".
func HTTPTransport(network string) http.RoundTripper {
	if network == "" {
		return nil
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = DialContextFor(network)
	return t
}

func bindNetwork(network, address string, c syscall.RawConn) error {
	if host, _, err := net.SplitHostPort(address); err == nil {
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return nil
		}
	}
	f := binder.Load()
	if f == nil {
		return fmt.Errorf("сеть %q: на этой платформе нельзя выбрать сеть для транспорта", network)
	}
	var err error
	if cerr := c.Control(func(fd uintptr) { err = (*f)(network, fd) }); cerr != nil {
		return cerr
	}
	if err != nil {
		return fmt.Errorf("сеть %q: %w", network, err)
	}
	return nil
}
