package mobile

import (
	"encoding/json"
	"fmt"
	"net"
	"testing"

	"openflux/share"
)

// Phone-as-exit to phone-as-client: the exit's link, decoded and turned
// into session specs the way the app does, must connect to that exit.
func TestExitShareLinkConnectsAClient(t *testing.T) {
	probe, _ := net.Listen("tcp", "127.0.0.1:0")
	_, port, _ := net.SplitHostPort(probe.Addr().String())
	probe.Close()

	const doc = "https://docs.example/d"
	exitSpecs := fmt.Sprintf(`[
		{"name":"direct","type":"direct","priority":100,"params":{"dial":"0.0.0.0:%s"}},
		{"name":"yandex","type":"yandex","url":%q,"priority":50},
		{"name":"oneme","type":"oneme","priority":10,"params":{"token":"secret-token"}}
	]`, port, doc)
	if _, err := ExitShareLink("127.0.0.1", "x"); err == nil {
		t.Fatal("a link was produced while the exit is not running")
	}
	if msg := StartSessionExit(exitSpecs, testSecret); msg != "" {
		t.Fatal(msg)
	}
	defer StopExit()

	link, err := ExitShareLink("127.0.0.1", "Pixel")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := ParseShareLink(link)
	if err != nil {
		t.Fatal(err)
	}
	var c share.Config
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	if c.Name != "Pixel" || !c.Negotiate || c.Context != doc || c.Secret != testSecret || len(c.Transports) != 2 ||
		c.Transports[0].Dial != "127.0.0.1:"+port || c.Transports[1].URL != doc {
		t.Fatalf("link carries %+v (MAX must be left out)", c)
	}

	// What the app builds from an imported profile.
	var specs []map[string]interface{}
	for _, tr := range c.Transports {
		specs = append(specs, map[string]interface{}{
			"name": tr.Type, "type": tr.Type, "url": tr.URL, "priority": tr.Priority,
			"params": map[string]interface{}{"dial": tr.Dial},
		})
	}
	clientSpecs, _ := json.Marshal(map[string]interface{}{"context": c.Context, "transports": specs})
	client, err := buildSession(string(clientSpecs), c.Secret, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Stop(); setAuthProxy(nil) }()
	if err := client.Start(); err != nil {
		t.Fatalf("client from the link could not reach the exit: %v", err)
	}
	waitUntil(t, "the exit to see the client", ExitIsConnected)
}

func TestParseShareLinkRejectsGarbage(t *testing.T) {
	if _, err := ParseShareLink("https://example.com"); err == nil {
		t.Fatal("accepted a non-openflux link")
	}
}

type fakeRooms string

func (f fakeRooms) RoomList() string { return string(f) }

// A cupsonline exit started without a room list creates its rooms at start;
// the link must carry them, or the client has nothing to join.
func TestExitShareLinkCarriesCreatedCupsRooms(t *testing.T) {
	tmpl := exitShareSession(`[{"name":"cupsonline","type":"cupsonline","priority":50}]`, "a shared secret of 32 characters")
	if tmpl == nil || len(tmpl.Transports) != 1 || tmpl.Transports[0].URL != "" {
		t.Fatalf("template %+v", tmpl)
	}
	exitNode.mu.Lock()
	exitNode.share, exitNode.rooms = tmpl, map[string]roomLister{"cupsonline": fakeRooms("WyJyb29tLTEiXQ")}
	exitNode.mu.Unlock()
	t.Cleanup(func() {
		exitNode.mu.Lock()
		exitNode.share, exitNode.rooms = nil, nil
		exitNode.mu.Unlock()
	})

	link, err := ExitShareLink("", "Phone")
	if err != nil {
		t.Fatal(err)
	}
	c, err := share.Decode(link)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Transports) != 1 || c.Transports[0].URL != "WyJyb29tLTEiXQ" {
		t.Fatalf("transports %+v", c.Transports)
	}
}
