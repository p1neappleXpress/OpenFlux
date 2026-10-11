package registry

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport"
	"github.com/p1neappleXpress/OpenFlux/transport/cupsonline"
	"github.com/p1neappleXpress/OpenFlux/transport/mailru"
	"github.com/p1neappleXpress/OpenFlux/transport/oneme"
	"github.com/p1neappleXpress/OpenFlux/transport/script"
	"github.com/p1neappleXpress/OpenFlux/transport/yandex"
)

func opts() Options { return Options{Base: transport.DefaultConfig()} }

func TestEveryNativeTypeBuilds(t *testing.T) {
	cases := map[string]interface{}{
		"":           &yandex.YandexDocsTransport{},
		"yandex":     &yandex.YandexDocsTransport{},
		"vyandex":    &yandex.YandexVolgaTransport{},
		"boards":     &yandex.BoardsTransport{},
		"mailru":     &mailru.MailruDocsTransport{},
		"cupsonline": &cupsonline.CupsonlineTransport{},
		"oneme":      &oneme.OneMeTransport{},
		"direct":     &transport.DirectTransport{},
	}
	for typ, want := range cases {
		got, err := New(typ, "https://example.test/doc", map[string]interface{}{"dial": "127.0.0.1:1", "token": "t", "uid": "5"}, opts())
		if err != nil {
			t.Errorf("%q: %v", typ, err)
			continue
		}
		if gt, wt := fmt.Sprintf("%T", got), fmt.Sprintf("%T", want); gt != wt {
			t.Errorf("%q built %s, want %s", typ, gt, wt)
		}
	}
}

// A Telemost carrier runs five lanes unless its profile says otherwise.
func TestTelemostLaneCount(t *testing.T) {
	t.Setenv("TELEMOST_STRIPES", "")
	cases := []struct {
		stripes interface{}
		want    int
	}{{nil, 5}, {"2", 2}, {float64(7), 7}, {"auto", 3}, {"nonsense", 5}}
	for _, c := range cases {
		params := map[string]interface{}{}
		if c.stripes != nil {
			params["stripes"] = c.stripes
		}
		tr, err := New("telemost", "https://telemost.yandex.ru/j/1", params, opts())
		if err != nil {
			t.Fatal(err)
		}
		st, ok := tr.(*transport.StripedTransport)
		if !ok {
			t.Fatalf("telemost built %T", tr)
		}
		if got := st.LaneCount(); got != c.want {
			t.Errorf("stripes=%v: %d lanes, want %d", c.stripes, got, c.want)
		}
	}
}

func TestEveryListedTypeIsKnown(t *testing.T) {
	for _, typ := range Types {
		_, err := New(typ, "x", map[string]interface{}{"dial": "127.0.0.1:1", "path": "/nope", "pubkey": strings.Repeat("00", 32)}, opts())
		if err != nil && strings.Contains(err.Error(), "unknown transport type") {
			t.Errorf("%q is listed in Types but New does not know it", typ)
		}
	}
	if _, err := New("telegram", "x", nil, opts()); err == nil || !strings.Contains(err.Error(), "unknown transport type") {
		t.Fatalf("an unknown type must be refused, got %v", err)
	}
}

// A cupsonline client never creates rooms; only an exit does (the transport
// is told its role here).
func TestCupsonlineRoleFollowsTheOptions(t *testing.T) {
	for _, exit := range []bool{false, true} {
		o := opts()
		o.IsExit = exit
		tr, err := New("cupsonline", "", nil, o)
		if err != nil {
			t.Fatal(err)
		}
		if tr == nil {
			t.Fatal("nil transport")
		}
	}
}

func TestDirectAddressesAndRoles(t *testing.T) {
	// A client dials; an exit listens, falling back to the one address a
	// profile keeps.
	o := opts()
	if _, err := New("direct", "", map[string]interface{}{"dial": "10.0.0.1:8445"}, o); err != nil {
		t.Fatal(err)
	}
	o.IsExit = true
	if _, err := New("direct", "", map[string]interface{}{"dial": "0.0.0.0:8445"}, o); err != nil {
		t.Fatal(err)
	}
	// Strict (the apps): no address is an error.
	o.StrictDirect = true
	if _, err := New("direct", "", nil, o); err == nil {
		t.Fatal("an exit without an address must be refused when strict")
	}
	o.IsExit = false
	if _, err := New("direct", "", nil, o); err == nil {
		t.Fatal("a client without an address must be refused when strict")
	}
	// Not strict (the CLI): it fails when it starts, as before.
	o.StrictDirect = false
	if _, err := New("direct", "", nil, o); err != nil {
		t.Fatalf("the CLI path must not refuse here: %v", err)
	}
}

func TestOnemeRoleFromParamsOrOptions(t *testing.T) {
	// "exit" in the params wins (a bool or "true"); otherwise the options say.
	for _, params := range []map[string]interface{}{{"exit": true}, {"exit": "true"}, {"exit": false}, nil} {
		if _, err := New("oneme", "", params, opts()); err != nil {
			t.Fatalf("%v: %v", params, err)
		}
	}
	if !roleParam(map[string]interface{}{"exit": "TRUE"}, "exit", false) || roleParam(map[string]interface{}{"exit": "no"}, "exit", true) {
		t.Fatal("roleParam must read \"true\" in any case and treat other strings as false")
	}
	if !roleParam(nil, "exit", true) {
		t.Fatal("without the param the default applies")
	}
}

func TestScriptNeedsAPathAndAKey(t *testing.T) {
	if _, err := New("script", "", map[string]interface{}{}, opts()); err == nil {
		t.Fatal("a script without a path must be refused")
	}
	if _, err := New("script", "", map[string]interface{}{"path": "/x.flux", "pubkey": "zz"}, opts()); err == nil {
		t.Fatal("a script with a bad key must be refused")
	}
}

func TestWithRoleCopiesAndKeepsWhatTheProfileSet(t *testing.T) {
	in := map[string]interface{}{"path": "p"}
	out := withRole(in, true)
	if out["exit"] != true || out["path"] != "p" {
		t.Fatalf("%v", out)
	}
	if _, set := in["exit"]; set {
		t.Fatal("the caller's map must not be changed")
	}
	if withRole(map[string]interface{}{"exit": false}, true)["exit"] != false {
		t.Fatal("a role the profile set must be kept")
	}
}

func TestLowMemoryShrinksTheQueue(t *testing.T) {
	o := opts()
	o.LowMemory = true
	if _, err := New("vyandex", "https://example.test/", nil, o); err != nil {
		t.Fatal(err)
	}
}

// What the user saved in a script's settings wizard reaches the script as
// cfg.params, next to the keys the core adds; the declared defaults fill in
// what was never set; and a setting cannot shadow a key the core owns.
func TestScriptSettingsReachTheScript(t *testing.T) {
	const src = `
var Transport = {
  info: function () {
    return { name: "settings-e2e", version: "1.0.0", params: [
      { key: "url", label: "U", type: "url" },
      { key: "token", label: "T", type: "secret" },
      { key: "retries", label: "R", type: "number", default: 3 },
      { key: "mode", label: "M", type: "select", options: ["a", "b"], default: "a" }
    ] };
  },
  open: function (cfg) { setState("connected"); raise("cfg", { json: JSON.stringify({ url: cfg.url, params: cfg.params }) }); },
  write: function () {}, close: function () {}
};`
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "s.js")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".sig", ed25519.Sign(priv, []byte(src)), 0o644); err != nil {
		t.Fatal(err)
	}
	params := map[string]interface{}{
		"path": path, "pubkey": hex.EncodeToString(pub), "name": "s",
		"settings": map[string]interface{}{"token": "a#b;c", "retries": "7", "exit": "hijack", "path": "/etc/passwd"},
	}
	tr, err := New("script", "https://doc.example/d", params, Options{Base: transport.DefaultConfig(), IsExit: false})
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan string, 1)
	tr.(*script.ScriptTransport).SetEventHandler(func(kind string, p map[string]interface{}) {
		if kind == "cfg" {
			got <- p["json"].(string)
		}
	})
	if err := tr.Start(); err != nil {
		t.Fatal(err)
	}
	defer tr.Stop()
	var cfg struct {
		URL    string                 `json:"url"`
		Params map[string]interface{} `json:"params"`
	}
	select {
	case s := <-got:
		if err := json.Unmarshal([]byte(s), &cfg); err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the script never opened")
	}
	if cfg.URL != "https://doc.example/d" {
		t.Errorf("cfg.url = %q", cfg.URL)
	}
	p := cfg.Params
	if p["token"] != "a#b;c" || p["retries"] != "7" {
		t.Errorf("saved settings: %v", p)
	}
	if p["mode"] != "a" {
		t.Errorf("an unset setting must show its declared default, got %q", p["mode"])
	}
	if p["path"] != path || p["exit"] != false {
		t.Errorf("a setting overrode a key the core owns: path %q exit %q", p["path"], p["exit"])
	}
}
