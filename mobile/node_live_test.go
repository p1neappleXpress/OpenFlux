package mobile

import (
	"encoding/json"
	"os"
	"testing"
)

// TestNodeLive checks a real channel: OPENFLUX_TEST_DOC is its document,
// OPENFLUX_TEST_KEY its key, OPENFLUX_TEST_DIRECT the node's direct
// host:port and OPENFLUX_TEST_EXPECT the address traffic must leave from.
func TestNodeLive(t *testing.T) {
	doc := os.Getenv("OPENFLUX_TEST_DOC")
	if doc == "" {
		t.Skip("OPENFLUX_TEST_DOC not set")
	}
	// From an address Yandex already challenges this is a SmartCaptcha;
	// the wizard shows it as a warning and relies on NodeVerify.
	t.Logf("check: %s", NodeCheckDocument(doc))
	specs, _ := json.Marshal(map[string]interface{}{
		"context": doc,
		"transports": []map[string]interface{}{
			{"name": "vyandex", "type": "vyandex", "url": doc, "priority": 100, "params": map[string]interface{}{}},
			{"name": "direct", "type": "direct", "priority": 50, "params": map[string]interface{}{"dial": os.Getenv("OPENFLUX_TEST_DIRECT")}},
		},
	})
	out := NodeVerify(string(specs), os.Getenv("OPENFLUX_TEST_KEY"), os.Getenv("OPENFLUX_TEST_EXPECT"), 60)
	if !okResult(t, out) {
		t.Fatalf("verify: %s", out)
	}
	t.Logf("verify: %s", out)
	if out := NodeVerify(string(specs), "0000000000000000000000000000000000000000000000000000000000000000", "", 25); okResult(t, out) {
		t.Fatalf("verify with a wrong key must fail: %s", out)
	}
}

func okResult(t *testing.T, out string) bool {
	var r struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("bad result %q: %v", out, err)
	}
	return r.OK
}

func TestNodeShareLinkRoundTrip(t *testing.T) {
	link, err := NodeShareLink("Моя нода", `[{"type":"vyandex","url":"https://docs.yandex.ru/edit/d/abcdefghijklmnopqrstuv"}]`, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "203.0.113.5", 30123)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := ParseShareLink(link)
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Negotiate  bool   `json:"negotiate"`
		Context    string `json:"context"`
		Transports []struct {
			Type, URL, Dial string
			Priority        int
		} `json:"transports"`
	}
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	if !c.Negotiate || c.Context == "" || len(c.Transports) != 2 || c.Transports[1].Dial != "203.0.113.5:30123" {
		t.Fatalf("unexpected config: %s", raw)
	}
}
