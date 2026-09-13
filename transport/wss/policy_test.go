package wss

import (
	"context"
	"errors"
	"net/netip"
	"reflect"
	"testing"
)

type fakeResolver struct {
	addresses []netip.Addr
	err       error
	calls     int
	network   string
	host      string
}

func (r *fakeResolver) LookupNetIP(_ context.Context, network, host string) ([]netip.Addr, error) {
	r.calls++
	r.network = network
	r.host = host
	return append([]netip.Addr(nil), r.addresses...), r.err
}

func newTestPolicy(t *testing.T, resolver ipResolver, denied []string, local ...string) *DestinationPolicy {
	t.Helper()
	localAddresses := make([]netip.Addr, 0, len(local))
	for _, raw := range local {
		localAddresses = append(localAddresses, netip.MustParseAddr(raw))
	}
	policy, err := newDestinationPolicy(denied, resolver, localAddresses)
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func TestDestinationPolicyPublicLiteral(t *testing.T) {
	resolver := &fakeResolver{}
	policy := newTestPolicy(t, resolver, nil)
	addresses, err := policy.Resolve(context.Background(), "8.8.8.8:443")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"8.8.8.8:443"}; !reflect.DeepEqual(addresses, want) {
		t.Fatalf("Resolve() = %v, want %v", addresses, want)
	}
	if resolver.calls != 0 {
		t.Fatalf("literal address unexpectedly used DNS %d times", resolver.calls)
	}
}

func TestDestinationPolicyResolvesOnceToLiterals(t *testing.T) {
	resolver := &fakeResolver{addresses: []netip.Addr{
		netip.MustParseAddr("8.8.8.8"),
		netip.MustParseAddr("1.1.1.1"),
		netip.MustParseAddr("8.8.8.8"),
	}}
	policy := newTestPolicy(t, resolver, nil)
	addresses, err := policy.Resolve(context.Background(), "Example.COM.:443")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"8.8.8.8:443", "1.1.1.1:443"}
	if !reflect.DeepEqual(addresses, want) {
		t.Fatalf("Resolve() = %v, want %v", addresses, want)
	}
	if resolver.calls != 1 || resolver.network != "ip4" || resolver.host != "Example.COM" {
		t.Fatalf("resolver calls=%d network=%q host=%q", resolver.calls, resolver.network, resolver.host)
	}
}

func TestDestinationPolicyDeniedAddressClasses(t *testing.T) {
	addresses := []string{
		"0.0.0.0",
		"10.1.2.3",
		"100.64.0.1",
		"127.0.0.1",
		"169.254.10.20",
		"172.16.0.1",
		"192.0.0.1",
		"192.0.2.10",
		"192.88.99.1",
		"192.168.1.1",
		"198.18.0.1",
		"198.51.100.10",
		"203.0.113.10",
		"224.0.0.1",
		"240.0.0.1",
		"255.255.255.255",
	}
	policy := newTestPolicy(t, &fakeResolver{}, nil)
	for _, address := range addresses {
		t.Run(address, func(t *testing.T) {
			_, err := policy.Resolve(context.Background(), address+":80")
			if !errors.Is(err, ErrPolicyDenied) {
				t.Fatalf("Resolve() error = %v, want ErrPolicyDenied", err)
			}
		})
	}
}

func TestDestinationPolicyConfiguredAndLocalDenials(t *testing.T) {
	policy := newTestPolicy(t, &fakeResolver{}, []string{"8.8.8.0/24"}, "1.1.1.1")
	for _, target := range []string{"8.8.8.8:53", "1.1.1.1:443"} {
		_, err := policy.Resolve(context.Background(), target)
		if !errors.Is(err, ErrPolicyDenied) {
			t.Fatalf("Resolve(%q) error = %v, want ErrPolicyDenied", target, err)
		}
	}
}

func TestDestinationPolicyRejectsDomainIfAnyAnswerIsDenied(t *testing.T) {
	resolver := &fakeResolver{addresses: []netip.Addr{
		netip.MustParseAddr("8.8.8.8"),
		netip.MustParseAddr("127.0.0.1"),
	}}
	policy := newTestPolicy(t, resolver, nil)
	_, err := policy.Resolve(context.Background(), "mixed.example:443")
	if !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("Resolve() error = %v, want ErrPolicyDenied", err)
	}
}

func TestDestinationPolicyRejectsIPv6(t *testing.T) {
	policy := newTestPolicy(t, &fakeResolver{}, nil)
	_, err := policy.Resolve(context.Background(), "[2001:4860:4860::8888]:53")
	if !errors.Is(err, ErrIPv6Unsupported) {
		t.Fatalf("Resolve() error = %v, want ErrIPv6Unsupported", err)
	}
}

func TestDestinationPolicyInvalidTargets(t *testing.T) {
	policy := newTestPolicy(t, &fakeResolver{}, nil)
	for _, target := range []string{
		"",
		"example.com",
		"example.com:0",
		"example.com:https",
		"-bad.example:443",
		"bad_.example:443",
		"0127.0.0.1:80",
	} {
		t.Run(target, func(t *testing.T) {
			_, err := policy.Resolve(context.Background(), target)
			if !errors.Is(err, ErrInvalidTarget) {
				t.Fatalf("Resolve() error = %v, want ErrInvalidTarget", err)
			}
		})
	}
}

func TestDestinationPolicyResolverFailure(t *testing.T) {
	resolver := &fakeResolver{err: errors.New("resolver unavailable")}
	policy := newTestPolicy(t, resolver, nil)
	_, err := policy.Resolve(context.Background(), "example.com:443")
	if !errors.Is(err, ErrResolveFailed) {
		t.Fatalf("Resolve() error = %v, want ErrResolveFailed", err)
	}
}

func TestDestinationPolicyRejectsInvalidDenyCIDR(t *testing.T) {
	_, err := newDestinationPolicy([]string{"bad"}, &fakeResolver{}, nil)
	if err == nil {
		t.Fatal("newDestinationPolicy() unexpectedly accepted invalid CIDR")
	}
	_, err = newDestinationPolicy([]string{"2001:db8::/32"}, &fakeResolver{}, nil)
	if err == nil {
		t.Fatal("newDestinationPolicy() unexpectedly accepted IPv6 CIDR")
	}
}
