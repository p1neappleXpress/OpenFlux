package wss

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

var (
	ErrInvalidTarget   = errors.New("invalid target")
	ErrIPv6Unsupported = errors.New("IPv6 is not supported")
	ErrResolveFailed   = errors.New("target resolution failed")
	ErrPolicyDenied    = errors.New("destination denied by policy")
)

type ipResolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// DestinationPolicy resolves a requested hostname once, checks every returned
// IPv4 address, and gives the caller only validated literal dial addresses.
type DestinationPolicy struct {
	resolver ipResolver
	denied   []netip.Prefix
	local    map[netip.Addr]struct{}
}

var alwaysDeniedIPv4 = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
}

// NewDestinationPolicy builds the production exit policy and snapshots all
// local interface addresses so the tunnel cannot dial back into the VPS.
func NewDestinationPolicy(denyCIDRs []string) (*DestinationPolicy, error) {
	local, err := systemLocalAddresses()
	if err != nil {
		return nil, fmt.Errorf("enumerate local interfaces: %w", err)
	}
	return newDestinationPolicy(denyCIDRs, net.DefaultResolver, local)
}

func newDestinationPolicy(denyCIDRs []string, resolver ipResolver, local []netip.Addr) (*DestinationPolicy, error) {
	if resolver == nil {
		return nil, errors.New("resolver is nil")
	}
	policy := &DestinationPolicy{
		resolver: resolver,
		local:    make(map[netip.Addr]struct{}, len(local)),
	}
	for _, address := range local {
		if address.Is4() {
			policy.local[address.Unmap()] = struct{}{}
		}
	}
	for _, raw := range denyCIDRs {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(raw))
		if err != nil {
			return nil, fmt.Errorf("parse deny CIDR %q: %w", raw, err)
		}
		if !prefix.Addr().Is4() {
			return nil, fmt.Errorf("deny CIDR %q is IPv6; this release is IPv4-only", raw)
		}
		policy.denied = append(policy.denied, prefix.Masked())
	}
	return policy, nil
}

// Resolve validates target and returns one or more literal IPv4 host:port
// addresses. The caller must dial these strings directly and must not resolve
// the original hostname again.
func (p *DestinationPolicy) Resolve(ctx context.Context, target string) ([]string, error) {
	if len(target) == 0 || len(target) > maxTargetLength {
		return nil, ErrInvalidTarget
	}
	host, portText, err := net.SplitHostPort(target)
	if err != nil || host == "" {
		return nil, ErrInvalidTarget
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		return nil, ErrInvalidTarget
	}

	if address, parseErr := netip.ParseAddr(host); parseErr == nil {
		if !address.Is4() {
			return nil, ErrIPv6Unsupported
		}
		address = address.Unmap()
		if err := p.validateAddress(address); err != nil {
			return nil, err
		}
		return []string{net.JoinHostPort(address.String(), portText)}, nil
	}
	if strings.Contains(host, ":") || looksLikeMalformedIPv4(host) || !validDomainName(host) {
		return nil, ErrInvalidTarget
	}

	host = strings.TrimSuffix(host, ".")
	addresses, err := p.resolver.LookupNetIP(ctx, "ip4", host)
	if err != nil || len(addresses) == 0 {
		return nil, ErrResolveFailed
	}

	seen := make(map[netip.Addr]struct{}, len(addresses))
	resolved := make([]string, 0, len(addresses))
	for _, address := range addresses {
		if !address.Is4() {
			return nil, ErrIPv6Unsupported
		}
		address = address.Unmap()
		if err := p.validateAddress(address); err != nil {
			return nil, err
		}
		if _, exists := seen[address]; exists {
			continue
		}
		seen[address] = struct{}{}
		resolved = append(resolved, net.JoinHostPort(address.String(), strconv.FormatUint(port, 10)))
	}
	if len(resolved) == 0 {
		return nil, ErrResolveFailed
	}
	return resolved, nil
}

func (p *DestinationPolicy) validateAddress(address netip.Addr) error {
	if !address.IsValid() || !address.Is4() || !address.IsGlobalUnicast() ||
		address.IsUnspecified() || address.IsLoopback() || address.IsPrivate() ||
		address.IsLinkLocalUnicast() || address.IsMulticast() {
		return ErrPolicyDenied
	}
	for _, prefix := range alwaysDeniedIPv4 {
		if prefix.Contains(address) {
			return ErrPolicyDenied
		}
	}
	if _, denied := p.local[address]; denied {
		return ErrPolicyDenied
	}
	for _, prefix := range p.denied {
		if prefix.Contains(address) {
			return ErrPolicyDenied
		}
	}
	return nil
}

func systemLocalAddresses() ([]netip.Addr, error) {
	interfaceAddresses, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	addresses := make([]netip.Addr, 0, len(interfaceAddresses))
	for _, interfaceAddress := range interfaceAddresses {
		prefix, err := netip.ParsePrefix(interfaceAddress.String())
		if err != nil {
			continue
		}
		addresses = append(addresses, prefix.Addr().Unmap())
	}
	return addresses, nil
}

func looksLikeMalformedIPv4(host string) bool {
	if !strings.Contains(host, ".") {
		return false
	}
	for _, char := range host {
		if (char < '0' || char > '9') && char != '.' {
			return false
		}
	}
	return true
}

func validDomainName(host string) bool {
	host = strings.TrimSuffix(host, ".")
	if len(host) == 0 || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || !isAlphaNumeric(label[0]) || !isAlphaNumeric(label[len(label)-1]) {
			return false
		}
		for i := 1; i < len(label)-1; i++ {
			if !isAlphaNumeric(label[i]) && label[i] != '-' {
				return false
			}
		}
	}
	return true
}

func isAlphaNumeric(char byte) bool {
	return (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9')
}
