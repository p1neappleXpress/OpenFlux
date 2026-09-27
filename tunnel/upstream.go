package tunnel

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/proxy"

	"openflux/utils"
)

// An L4 exit can open its TCP connections through a SOCKS5 proxy on the
// exit host, such as an xray or sing-box inbound with its own routing rules
// (--upstream-proxy, #72). UDP still leaves the exit directly: the proxy
// would need SOCKS5 UDP ASSOCIATE, and dropping UDP would break DNS for
// clients that resolve through the tunnel.

type upstreamDialer struct {
	addr string
	d    proxy.ContextDialer
}

var exitUpstream atomic.Pointer[upstreamDialer]

// SetExitUpstream routes the L4 exit's TCP connections through the SOCKS5
// proxy at spec: host:port, :port (localhost) or socks5://[user:pass@]host:port.
// "" or "direct" dials directly.
func SetExitUpstream(spec string) error {
	spec = strings.TrimSpace(spec)
	if spec == "" || strings.EqualFold(spec, "direct") {
		exitUpstream.Store(nil)
		return nil
	}
	addr, auth, err := parseSOCKS5URL(spec)
	if err != nil {
		return err
	}
	d, err := proxy.SOCKS5("tcp", addr, auth, &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second})
	if err != nil {
		return fmt.Errorf("upstream proxy %s: %w", addr, err)
	}
	cd, ok := d.(proxy.ContextDialer)
	if !ok {
		return fmt.Errorf("upstream proxy %s: dialer has no DialContext", addr)
	}
	exitUpstream.Store(&upstreamDialer{addr: addr, d: cd})
	return nil
}

// ExitUpstream returns the proxy address set by SetExitUpstream, or "".
func ExitUpstream() string {
	if u := exitUpstream.Load(); u != nil {
		return u.addr
	}
	return ""
}

func parseSOCKS5URL(spec string) (string, *proxy.Auth, error) {
	if !strings.Contains(spec, "://") {
		spec = "socks5://" + spec
	}
	u, err := url.Parse(spec)
	if err != nil {
		return "", nil, fmt.Errorf("upstream proxy: %w", err)
	}
	if u.Scheme != "socks5" && u.Scheme != "socks5h" {
		return "", nil, fmt.Errorf("upstream proxy: only socks5 is supported, got %q", u.Scheme)
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil || port == "" {
		return "", nil, fmt.Errorf("upstream proxy: want host:port, got %q", u.Host)
	}
	if host == "" {
		host = "127.0.0.1"
	}
	var auth *proxy.Auth
	if u.User != nil {
		auth = &proxy.Auth{User: u.User.Username()}
		auth.Password, _ = u.User.Password()
	}
	return net.JoinHostPort(host, port), auth, nil
}

// dialExitTCP opens an exit connection to dest, through the upstream proxy
// when one is set.
func dialExitTCP(dest string) (net.Conn, error) {
	if u := exitUpstream.Load(); u != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return u.d.DialContext(ctx, "tcp", dest)
	}
	return net.DialTimeout("tcp", dest, 10*time.Second)
}

var udpBypassNote sync.Once

func noteUDPBypassesUpstream() {
	if exitUpstream.Load() == nil {
		return
	}
	udpBypassNote.Do(func() {
		utils.Infof("[EXIT] UDP is not sent through --upstream-proxy; it leaves this host directly")
	})
}
