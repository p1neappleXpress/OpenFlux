package transportstack

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

func TestVolgaRoutesResolveActualHosts(t *testing.T) {
	hosts, err := BypassHosts("vyandex", testURL)
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"volga.yandex.ru", "push.yandex.ru", "document.invalid"} {
		if !slices.Contains(hosts, host) {
			t.Fatalf("missing endpoint %s", host)
		}
	}
	plan, err := ResolveBypassIPv4(context.Background(), "vyandex", hosts, func(_ context.Context, host string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("192.0.2.10")}, {IP: net.ParseIP("2001:db8::1")}}, nil
	}, func(context.Context, string) ([]net.IPAddr, error) {
		t.Error("system DNS used despite DoT success")
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ips := plan.Addresses()
	if !slices.Contains(ips, "192.0.2.10") || !slices.Contains(ips, "77.88.8.8") || slices.Contains(ips, "2001:db8::1") {
		t.Fatal("incorrect route families/resolver routes")
	}
}

func TestRouteFailureIsSanitized(t *testing.T) {
	_, err := ResolveBypassIPv4(context.Background(), "vyandex", []string{"document.invalid"}, unavailableDNS, nil)
	if err == nil || err.Error() == "secret failure detail" {
		t.Fatal("lookup failure leaked or ignored")
	}
}

func unavailableDNS(context.Context, string) ([]net.IPAddr, error) {
	return nil, errors.New("secret failure detail")
}
func testAnswer(ip string) []net.IPAddr { return []net.IPAddr{{IP: net.ParseIP(ip)}} }

func TestYandexSystemBootstrapFallbackAndPinnedCarrier(t *testing.T) {
	for _, kind := range []string{"yandex", "vyandex"} {
		t.Run(kind, func(t *testing.T) {
			hosts, _ := BypassHosts(kind, "https://disk.yandex.ru/")
			var systemCalls atomic.Int32
			plan, err := ResolveBypassIPv4(context.Background(), kind, hosts,
				func(ctx context.Context, host string) ([]net.IPAddr, error) {
					deadline, ok := ctx.Deadline()
					if !ok || time.Until(deadline) > bootstrapLookupTimeout {
						t.Error("unbounded DoT lookup")
					}
					return nil, context.DeadlineExceeded // TCP/853 blackhole
				}, func(_ context.Context, host string) ([]net.IPAddr, error) {
					if !knownYandexBootstrap(host) {
						t.Error("system fallback for unknown endpoint")
					}
					systemCalls.Add(1)
					return testAnswer("192.0.2.10"), nil
				})
			if err != nil {
				t.Fatal(err)
			}
			before := systemCalls.Load()
			if before == 0 {
				t.Fatal("system fallback not attempted")
			}
			for _, host := range hosts {
				a, b := net.Pipe()
				conn, err := plan.dialContext(context.Background(), "tcp", host+":443", func(_ context.Context, network, address string) (net.Conn, error) {
					if network != "tcp4" || address != "192.0.2.10:443" {
						t.Error("carrier did not use pinned numeric address")
					}
					return a, nil
				})
				a.Close()
				b.Close()
				if err != nil || conn != a {
					t.Fatal("carrier still needs DoT after bootstrap")
				}
			}
			if systemCalls.Load() != before {
				t.Fatal("system lookup after bootstrap")
			}
			if !slices.Contains(plan.Routes(), IPv4Route{"192.0.2.10", "255.255.255.255"}) {
				t.Fatal("missing dynamic exclusion")
			}
		})
	}
}

func TestPartialYandexFailureUsesGuardedPrefixes(t *testing.T) {
	hosts, _ := BypassHosts("vyandex", "https://disk.yandex.ru/")
	var activated atomic.Bool
	secure := func(_ context.Context, host string) ([]net.IPAddr, error) {
		if host == "push.yandex.ru" || host == "volga.yandex.ru" {
			if !activated.Load() {
				return nil, errors.New("temporary DNS failure")
			}
			return testAnswer("77.88.0.42"), nil // synthetic address within an inherited CIDR
		}
		return testAnswer("192.0.2.10"), nil
	}
	plan, err := ResolveBypassIPv4(context.Background(), "vyandex", hosts, secure, unavailableDNS)
	if err != nil {
		t.Fatal("partial failure aborted safe bootstrap:", err)
	}
	if len(plan.prefixes) != 13 || !slices.Contains(plan.Routes(), IPv4Route{"77.88.0.0", "255.255.192.0"}) {
		t.Fatal("lost inherited Yandex safety net")
	}
	activated.Store(true)
	for _, host := range []string{"volga.yandex.ru", "push.yandex.ru"} {
		a, b := net.Pipe()
		conn, err := plan.dialContext(context.Background(), "tcp", host+":443", func(_ context.Context, _, address string) (net.Conn, error) {
			if address != "77.88.0.42:443" {
				t.Error("unexpected carrier address")
			}
			return a, nil
		})
		a.Close()
		b.Close()
		if err != nil || conn != a {
			t.Fatal("covered carrier could not recover")
		}
	}
	if plan.HostRoutesOnly().covers(netip.MustParseAddr("77.88.0.42")) {
		t.Fatal("V1 /32-only API authorized a CIDR it cannot return")
	}
}

func TestUnknownEndpointAndMAXRemainFailClosed(t *testing.T) {
	for _, tc := range []struct{ kind, host string }{
		{"vyandex", "uncovered.invalid"}, {"yandex", "unknown.yandex.ru"},
		{"vyandex", "push.yandex.ru.attacker.invalid"}, {"oneme", "ws-api.oneme.ru"},
	} {
		t.Run(tc.kind+"/"+tc.host, func(t *testing.T) {
			_, err := ResolveBypassIPv4(context.Background(), tc.kind, []string{tc.host}, unavailableDNS,
				func(context.Context, string) ([]net.IPAddr, error) {
					t.Error("unauthorized system fallback")
					return testAnswer("192.0.2.10"), nil
				})
			if err != errBypass {
				t.Fatal("uncovered endpoint did not fail safely")
			}
		})
	}
	_, err := ResolveBypassIPv4(context.Background(), "oneme", []string{"ws-api.oneme.ru"},
		func(context.Context, string) ([]net.IPAddr, error) { return testAnswer("2001:db8::1"), nil }, nil)
	if err == nil {
		t.Fatal("MAX silently accepted no IPv4 exclusion")
	}
}

func TestRebindingCannotDialUncoveredIP(t *testing.T) {
	var activated atomic.Bool
	secure := func(context.Context, string) ([]net.IPAddr, error) {
		if activated.Load() {
			return testAnswer("203.0.113.20"), nil
		}
		return testAnswer("192.0.2.10"), nil
	}
	plan, err := ResolveBypassIPv4(context.Background(), "vyandex", []string{"push.yandex.ru"}, secure, nil)
	if err != nil {
		t.Fatal(err)
	}
	activated.Store(true)
	for _, host := range []string{"push.yandex.ru", "new-backend.invalid", "203.0.113.20"} {
		_, err := plan.dialContext(context.Background(), "tcp", host+":443", func(_ context.Context, _, address string) (net.Conn, error) {
			if address != "192.0.2.10:443" {
				t.Error("uncovered address reached the network")
			}
			return nil, errors.New("old endpoint unavailable")
		})
		if err != errBypass {
			t.Fatal("unsafe dial or error details escaped")
		}
	}
}

func TestCancelledBootstrapDoesNotFallBackToPrefixes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ResolveBypassIPv4(ctx, "vyandex", []string{"push.yandex.ru"}, unavailableDNS, unavailableDNS)
	if err == nil {
		t.Fatal("cancelled setup succeeded")
	}
}
