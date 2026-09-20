package transportstack

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// BypassHosts lists control/relay endpoints used by the actual constructors.
// Resolve before installing a default TUN route. No deployment IPs live here.
func BypassHosts(kind, documentURL string) ([]string, error) {
	u, err := url.Parse(documentURL)
	if err != nil {
		return nil, errors.New("invalid document URL")
	}
	var hosts []string
	switch kind {
	case "yandex", "vyandex":
		if u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return nil, errors.New("invalid document URL")
		}
		hosts = []string{u.Hostname(), "disk.yandex.ru", "docs.yandex.ru", "docviewer.yandex.ru"}
		if kind == "vyandex" {
			hosts = append(hosts, "volga.yandex.ru", "push.yandex.ru")
		}
	case "oneme":
		hosts = []string{"ws-api.oneme.ru", "web.max.ru"}
	default:
		return nil, errors.New("unsupported transport")
	}
	return hosts, nil
}

type IPLookup func(context.Context, string) ([]net.IPAddr, error)

// These are the existing PacketTunnelProvider exclusions, moved here so route
// installation and the carrier dial guard use exactly the same safety net.
// They are not a DNS assertion: every actual dial still checks its numeric IP.
var yandexBypassCIDRs = [...]string{
	"5.45.192.0/18", "5.255.192.0/18", "37.9.64.0/18", "37.140.128.0/18",
	"77.88.0.0/18", "84.201.128.0/18", "87.250.224.0/19", "90.156.176.0/22",
	"93.158.128.0/18", "95.108.128.0/17", "100.43.64.0/19",
	"178.154.128.0/17", "213.180.192.0/19",
}

const bootstrapLookupTimeout = 2 * time.Second

var errBypass = errors.New("cannot safely resolve transport bypass endpoints")

type IPv4Route struct {
	Destination string `json:"destination"`
	Mask        string `json:"mask"`
}

// BypassPlan is an immutable route snapshot. Only a Yandex carrier receives its
// dialer. The system resolver is deliberately NOT retained after bootstrap.
type BypassPlan struct {
	hostIPs   map[string][]netip.Addr
	addresses map[netip.Addr]bool
	prefixes  []netip.Prefix
	secure    IPLookup
}

func canonicalHost(host string) string { return strings.ToLower(strings.TrimSuffix(host, ".")) }

// Exact control endpoints only: neither a suffix match nor a user destination
// grants permission for system DNS or for unresolved-host prefix fallback.
func knownYandexBootstrap(host string) bool {
	switch canonicalHost(host) {
	case "disk.yandex.ru", "docs.yandex.ru", "docviewer.yandex.ru", "volga.yandex.ru", "push.yandex.ru":
		return true
	}
	return false
}

func lookupIPv4(ctx context.Context, host string, lookup IPLookup) []netip.Addr {
	if lookup == nil || ctx.Err() != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, bootstrapLookupTimeout)
	defer cancel()
	ips, err := lookup(ctx, host)
	if err != nil || ctx.Err() != nil {
		return nil
	}
	seen := make(map[netip.Addr]bool)
	var result []netip.Addr
	for _, ip := range ips {
		addr, ok := netip.AddrFromSlice(ip.IP)
		if !ok {
			continue
		}
		addr = addr.Unmap()
		if addr.Is4() && addr.IsGlobalUnicast() && !seen[addr] {
			seen[addr] = true
			result = append(result, addr)
		}
	}
	return result
}

// ResolveBypassIPv4 runs BEFORE the default TUN route is installed. Prefer DoT;
// allow system DNS only for exact Yandex bootstrap endpoints. A failed known
// endpoint may rely on the existing CIDRs, but the carrier must then use this
// plan's guarded dialer. Unknown endpoints and MAX remain fail-closed.
func ResolveBypassIPv4(ctx context.Context, kind string, hosts []string, secure, system IPLookup) (*BypassPlan, error) {
	yandex := kind == "yandex" || kind == "vyandex"
	if (!yandex && kind != "oneme") || len(hosts) == 0 || len(hosts) > 16 || secure == nil {
		return nil, errBypass
	}
	p := &BypassPlan{hostIPs: make(map[string][]netip.Addr), addresses: make(map[netip.Addr]bool), secure: secure}
	for _, ip := range []string{"77.88.8.8", "8.8.8.8", "1.1.1.1"} {
		p.addresses[netip.MustParseAddr(ip)] = true
	}
	if yandex {
		for _, cidr := range yandexBypassCIDRs {
			p.prefixes = append(p.prefixes, netip.MustParsePrefix(cidr))
		}
	}
	type result struct {
		host string
		ips  []netip.Addr
	}
	jobs := make(chan string, len(hosts))
	results := make(chan result, len(hosts))
	seen := make(map[string]bool)
	for _, host := range hosts {
		host = canonicalHost(host)
		if !seen[host] {
			seen[host] = true
			jobs <- host
		}
	}
	close(jobs)
	var workers sync.WaitGroup
	// At most two concurrent resolver/TLS operations, bounded startup latency
	// even when TCP/853 silently blackholes every request.
	for i := 0; i < 2; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for host := range jobs {
				ips := lookupIPv4(ctx, host, secure)
				if len(ips) == 0 && yandex && knownYandexBootstrap(host) {
					ips = lookupIPv4(ctx, host, system)
				}
				results <- result{host, ips}
			}
		}()
	}
	workers.Wait()
	close(results)
	if ctx.Err() != nil {
		return nil, errBypass
	}
	for r := range results {
		if len(r.ips) == 0 && !(yandex && knownYandexBootstrap(r.host)) {
			return nil, errBypass
		}
		p.hostIPs[r.host] = r.ips
		for _, ip := range r.ips {
			p.addresses[ip] = true
		}
	}
	return p, nil
}

func (p *BypassPlan) Addresses() []string {
	ips := make([]string, 0, len(p.addresses))
	for ip := range p.addresses {
		ips = append(ips, ip.String())
	}
	sort.Strings(ips)
	return ips
}

// HostRoutesOnly is for the old C API, which cannot return CIDRs. Never allow
// a prefix-only dial when the caller was given only a list of /32 exclusions.
func (p *BypassPlan) HostRoutesOnly() *BypassPlan {
	copy := *p
	copy.prefixes = nil
	return &copy
}

func (p *BypassPlan) Routes() []IPv4Route {
	routes := make([]IPv4Route, 0, len(p.prefixes)+len(p.addresses))
	for _, prefix := range p.prefixes {
		routes = append(routes, IPv4Route{prefix.Addr().String(), net.IP(net.CIDRMask(prefix.Bits(), 32)).String()})
	}
	for _, ip := range p.Addresses() {
		routes = append(routes, IPv4Route{ip, "255.255.255.255"})
	}
	return routes
}

func (p *BypassPlan) covers(ip netip.Addr) bool {
	if p.addresses[ip] {
		return true
	}
	for _, prefix := range p.prefixes {
		if prefix.Contains(ip) {
			return true
		}
	}
	return false
}

// DialContext uses cached bootstrap answers even when external DoT is blocked.
// Refreshes use secure DNS only; a newly discovered or rotated IP MUST already
// be excluded. Dial numeric IPv4 addresses, preserving HTTP Host and TLS SNI in
// the caller, so a second DNS lookup cannot rebind the connection into the TUN.
func (p *BypassPlan) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return p.dialContext(ctx, network, address, d.DialContext)
}

func (p *BypassPlan) dialContext(ctx context.Context, network, address string, dial func(context.Context, string, string) (net.Conn, error)) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || (network != "tcp" && network != "tcp4") {
		return nil, errBypass
	}
	tried := make(map[netip.Addr]bool)
	try := func(ips []netip.Addr) net.Conn {
		for _, ip := range ips {
			if ctx.Err() != nil || tried[ip] || !p.covers(ip) {
				continue
			}
			tried[ip] = true
			if conn, err := dial(ctx, "tcp4", net.JoinHostPort(ip.String(), port)); err == nil {
				return conn
			}
		}
		return nil
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if conn := try([]netip.Addr{ip.Unmap()}); conn != nil {
			return conn, nil
		}
		return nil, errBypass
	}
	host = canonicalHost(host)
	if conn := try(p.hostIPs[host]); conn != nil {
		return conn, nil
	}
	if conn := try(lookupIPv4(ctx, host, p.secure)); conn != nil {
		return conn, nil
	}
	return nil, errBypass
}
