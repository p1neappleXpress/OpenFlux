package yandex

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sort"
	"time"

	"github.com/p1neappleXpress/OpenFlux/netbind"
)

// The tunnel itself is IPv4: the L3 exit's raw sockets, the client's gVisor
// stack and the L4 exit's re-dials all carry IPv4 only. A check the node asks
// the client to pass (Yandex's SmartCaptcha) is shown to the client's browser
// through that tunnel, so Yandex sees the node's IPv4 address, and the
// "spravka" it issues is bound to that address. The node's own requests to
// Yandex must therefore leave from the same address family. A dual-stack Go
// dial goes out over IPv6 first wherever the host has an AAAA route, and
// Yandex then rejects the browser's spravka for the node's IPv6 address: the
// node is asked for the check again, over and over, whatever the client sends.
//
// Every connection of the Yandex carriers (document fetch, captcha PoW,
// WebSocket, Volga relay) dials through dialIPv4First: IPv4 addresses first,
// IPv6 only when none of them answers.

// carrierDialTimeout bounds one connection attempt, so a dead IPv4 route
// falls through to IPv6 in time instead of eating the whole connect budget.
const carrierDialTimeout = 10 * time.Second

// lookupIPAddr is the resolver; replaced in tests.
var lookupIPAddr = net.DefaultResolver.LookupIPAddr

// dialIPv4First is a DialContext that tries a host's IPv4 addresses before its
// IPv6 ones, on the default route (the package's global binding).
func dialIPv4First(ctx context.Context, network, address string) (net.Conn, error) {
	return dialIPv4From(ctx, "", network, address)
}

// dialIPv4FirstOn is dialIPv4First for a carrier bound to a network: the same
// order, through netbind.DialerFor(bind), so a carrier bound to a network keeps its own
// interface ("" is the default route).
func dialIPv4FirstOn(bind string) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		return dialIPv4From(ctx, bind, network, address)
	}
}

func dialIPv4From(ctx context.Context, bind, network, address string) (net.Conn, error) {
	if network != "tcp" {
		return netbind.DialContextFor(bind)(ctx, network, address)
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil || net.ParseIP(host) != nil {
		return netbind.DialContextFor(bind)(ctx, network, address)
	}
	addrs, err := lookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("lookup %s: no addresses", host)
	}
	// Stable: the resolver's own order is kept inside each family.
	sort.SliceStable(addrs, func(i, j int) bool {
		return addrs[i].IP.To4() != nil && addrs[j].IP.To4() == nil
	})
	var firstErr error
	for _, a := range addrs {
		netw := "tcp6"
		if a.IP.To4() != nil {
			netw = "tcp4"
		}
		conn, err := netbind.DialerFor(bind, carrierDialTimeout).DialContext(ctx, netw, net.JoinHostPort(a.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		if firstErr == nil {
			firstErr = err
		}
		if ctx.Err() != nil {
			break
		}
	}
	return nil, firstErr
}

// carrierTransport is the default HTTP transport dialing through dialIPv4FirstOn(bind).
func carrierTransport(bind string) *http.Transport {
	if dt, ok := http.DefaultTransport.(*http.Transport); ok {
		t := dt.Clone()
		t.DialContext = dialIPv4FirstOn(bind)
		return t
	}
	return &http.Transport{DialContext: dialIPv4FirstOn(bind), ForceAttemptHTTP2: true}
}
