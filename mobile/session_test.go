package mobile

import (
	"encoding/json"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport"
)

const testSecret = "mobile session test secret"

// The phone must derive the same encryption context as an exit started with
// --url=<doc> and reach it over direct.
func TestBuildSessionConnectsOverDirect(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	const doc = "https://docs.example/d"
	exit, err := transport.NewSession(transport.PeerParameters{
		Capabilities:  transport.CapabilityIPv4 | transport.CapabilityTCP | transport.CapabilityUDP,
		MaxPacketSize: transport.MaxNegotiatedPacket,
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	dcfg := transport.DefaultDirectConfig()
	dcfg.ListenAddr, dcfg.IsExit = addr, true
	if err := exit.AddTransport("direct", transport.NewDirectTransport(transport.DefaultConfig(), dcfg), testSecret, doc, 100); err != nil {
		t.Fatal(err)
	}
	if err := exit.Start(); err != nil {
		t.Fatal(err)
	}
	defer exit.Stop()

	// yandex never connects here (no such document); direct carries it.
	specs := fmt.Sprintf(`[
		{"name":"direct","type":"direct","priority":100,"params":{"dial":%q}},
		{"name":"yandex","type":"yandex","url":%q,"priority":50}
	]`, addr, doc)
	phone, err := buildSession(specs, testSecret, false)
	if err != nil {
		t.Fatal(err)
	}
	defer phone.Stop()
	if err := phone.Start(); err != nil {
		t.Fatalf("handshake over direct: %v", err)
	}
	if got := CurrentTransport(); got != "direct" {
		t.Fatalf("CurrentTransport = %q, want direct", got)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !exit.IsConnected() {
		if time.Now().After(deadline) {
			t.Fatal("exit did not see the phone")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func contextOf(t *testing.T, specs []sessionSpec) string {
	t.Helper()
	b, err := json.Marshal(specs)
	if err != nil {
		t.Fatal(err)
	}
	_, ctx, _, err := parseSessionSpecs(string(b))
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func TestSessionContextPrefersHighestPriorityURL(t *testing.T) {
	specs := []sessionSpec{
		{Name: "mailru", Type: "mailru", URL: "https://cloud.example/m", Priority: 10},
		{Name: "direct", Type: "direct", Priority: 100},
		{Name: "yandex", Type: "yandex", URL: "https://docs.example/d", Priority: 50},
	}
	if got := contextOf(t, specs); got != "https://docs.example/d" {
		t.Fatalf("context = %q", got)
	}
	if got := contextOf(t, []sessionSpec{{Name: "direct", Type: "direct"}}); got != "http://#" {
		t.Fatalf("context without URLs = %q", got)
	}
}

// The exit creates the cupsonline room list only when it starts, so the
// core leaves it out of the context; the phone must too.
func TestSessionContextSkipsCupsonline(t *testing.T) {
	specs := []sessionSpec{
		{Name: "cupsonline", Type: "cupsonline", URL: "room-list", Priority: 100},
		{Name: "yandex", Type: "yandex", URL: "https://docs.example/d", Priority: 50},
	}
	if got := contextOf(t, specs); got != "https://docs.example/d" {
		t.Fatalf("context = %q", got)
	}
	if got := contextOf(t, specs[:1]); got != "http://#" {
		t.Fatalf("context of a cupsonline-only session = %q", got)
	}
}

// A link's explicit context wins, and the derived one stays an alternate.
func TestSessionContextFromLink(t *testing.T) {
	_, ctx, alts, err := parseSessionSpecs(`{"context":"https://x/ctx","transports":[{"name":"yandex","type":"yandex","url":"https://docs.example/d","priority":100}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if ctx != "https://x/ctx" {
		t.Fatalf("context = %q", ctx)
	}
	found := false
	for _, a := range alts {
		found = found || a == "https://docs.example/d"
	}
	if !found {
		t.Fatalf("derived context missing from alternates %q", alts)
	}
}

// A classic exit's link names the context the classic client derives.
func TestClassicShareContext(t *testing.T) {
	if c := exitShareClassic("mailru", "https://cloud.example/m", "0123456789abcdef", "batched"); c.Context != "https://cloud.example/m" {
		t.Fatalf("mailru context = %q", c.Context)
	}
	if c := exitShareClassic("cupsonline", "room-list", "0123456789abcdef", "batched"); c.Context != "http://#" {
		t.Fatalf("cupsonline context = %q", c.Context)
	}
}
