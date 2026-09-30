package mobile

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport"
	"github.com/p1neappleXpress/OpenFlux/transport/manager"
	"github.com/p1neappleXpress/OpenFlux/tunnel"
)

// stubCarrier is a cookie-carrying transport on the exit that never
// connects: the one stuck on a captcha.
type stubCarrier struct {
	mu  sync.Mutex
	jar map[string]string
}

func (s *stubCarrier) Start() error                    { return nil }
func (s *stubCarrier) Stop() error                     { return nil }
func (s *stubCarrier) Send([]byte) error               { return nil }
func (s *stubCarrier) Receive(func([]byte))            {}
func (s *stubCarrier) IsConnected() bool               { return false }
func (s *stubCarrier) Stats() transport.TransportStats { return transport.TransportStats{} }

func (s *stubCarrier) FetchCookies() (map[string]string, error) { return nil, nil }
func (s *stubCarrier) ApplyCookies(jar map[string]string) error {
	s.mu.Lock()
	s.jar = jar
	s.mu.Unlock()
	return nil
}
func (s *stubCarrier) applied() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.jar
}

func lanIPv4(t *testing.T) net.IP {
	t.Helper()
	ifaces, _ := net.Interfaces()
	for _, iface := range ifaces {
		if iface.Flags&(net.FlagUp|net.FlagLoopback|net.FlagPointToPoint) != net.FlagUp {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			if ip, _, err := net.ParseCIDR(a.String()); err == nil && ip.To4() != nil && !ip.IsLinkLocalUnicast() {
				return ip.To4()
			}
		}
	}
	t.Skip("no non-loopback IPv4 interface")
	return nil
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The exit reports a captcha on its document transport; the phone gets a
// pending check with a proxy, the page loads through the tunnel and the
// exit, and the cookies land on the exit's transport.
func TestExitCaptchaSolvedThroughTunnel(t *testing.T) {
	web := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "captcha page")
	}))
	ln, err := net.Listen("tcp4", net.JoinHostPort(lanIPv4(t).String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	web.Listener = ln
	web.Start()
	defer web.Close()

	probe, _ := net.Listen("tcp", "127.0.0.1:0")
	directAddr := probe.Addr().String()
	probe.Close()

	const doc = "https://docs.example/d"
	exitSess, err := transport.NewSession(transport.PeerParameters{
		Capabilities:  transport.CapabilityIPv4 | transport.CapabilityTCP | transport.CapabilityUDP,
		MaxPacketSize: transport.MaxNegotiatedPacket,
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	exit := manager.New(exitSess, nil, testSecret, doc)
	dcfg := transport.DefaultDirectConfig()
	dcfg.ListenAddr, dcfg.IsExit = directAddr, true
	direct := transport.NewDirectTransport(transport.DefaultConfig(), dcfg)
	if err := exitSess.AddTransport("direct", direct, testSecret, doc, 100); err != nil {
		t.Fatal(err)
	}
	if err := exit.Add("direct", "direct", direct, 100, nil); err != nil {
		t.Fatal(err)
	}
	stuck := &stubCarrier{}
	if err := exit.Add("yandex", "yandex", stuck, 50, stuck); err != nil {
		t.Fatal(err)
	}
	exitSess.SetControlHandler(exit.DispatchControl)
	exitTunnel := tunnel.NewTCPTunnelMode(exit, true, tunnel.ExitModeL4)
	defer exitTunnel.Close()
	if err := exit.Start(); err != nil {
		t.Fatal(err)
	}
	defer exit.Stop()

	specs := fmt.Sprintf(`[{"name":"direct","type":"direct","priority":100,"params":{"dial":%q}},
		{"name":"yandex","type":"yandex","url":%q,"priority":50}]`, directAddr, doc)
	phone, err := buildSession(specs, testSecret, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = phone.Stop(); setAuthProxy(nil) }()
	if err := phone.Start(); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the exit to see the phone", exit.IsConnected)

	exit.NotifyCaptcha("yandex", web.URL, "smartcaptcha")
	waitUntil(t, "a pending exit check", func() bool {
		return PendingCaptchaURL() == web.URL && PendingCaptchaProxy() != ""
	})

	proxyURL := &url.URL{Scheme: "http", Host: PendingCaptchaProxy()}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}, Timeout: 10 * time.Second}
	resp, err := client.Get(web.URL)
	if err != nil {
		t.Fatalf("page through the tunnel: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "captcha page") {
		t.Fatalf("body = %q", body)
	}

	if msg := SubmitCaptchaCookies("spravka=s1; yandexuid=7"); msg != "" {
		t.Fatal(msg)
	}
	waitUntil(t, "cookies on the exit's transport", func() bool { return stuck.applied()["spravka"] == "s1" })
	if PendingCaptchaURL() != "" || PendingCaptchaProxy() != "" {
		t.Fatal("pending check not cleared after submit")
	}
}

func TestCancelledExitCheckIsSnoozed(t *testing.T) {
	t.Cleanup(func() {
		captcha.mu.Lock()
		captcha.snooze = nil
		captcha.mu.Unlock()
	})
	captcha.mu.Lock()
	captcha.url, captcha.remoteName, captcha.proxy = "https://docs.example/d", "yandex", "127.0.0.1:1"
	captcha.mu.Unlock()
	CancelCaptcha()
	captcha.mu.Lock()
	until := captcha.snooze["yandex"]
	captcha.mu.Unlock()
	if time.Until(until) < remoteSnooze-time.Minute {
		t.Fatalf("snoozed until %v", until)
	}
}
