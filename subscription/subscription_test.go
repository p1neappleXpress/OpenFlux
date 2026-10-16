package subscription

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/p1neappleXpress/OpenFlux/share"
)

const pin = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestParseInviteForms(t *testing.T) {
	want := Invite{URL: "https://203.0.113.5:8443/sub/TOKEN", Pin: pin, Name: "Мой сервис"}
	for name, in := range map[string]string{
		"link":      want.Link(),
		"plain":     "https://203.0.113.5:8443/sub/TOKEN#pin=" + pin,
		"colons":    "https://203.0.113.5:8443/sub/TOKEN#pin=sha256:" + strings.ToUpper(strings.Join(chunks(pin), ":")),
		"spaces":    "  " + want.Link() + "\n",
		"fragment":  "openflux://sub?u=https%3A%2F%2F203.0.113.5%3A8443%2Fsub%2FTOKEN%23x&p=" + pin + "&n=%D0%9C%D0%BE%D0%B9+%D1%81%D0%B5%D1%80%D0%B2%D0%B8%D1%81",
		"prefix p:": "openflux://sub?u=https%3A%2F%2F203.0.113.5%3A8443%2Fsub%2FTOKEN&p=sha256:" + pin + "&n=%D0%9C%D0%BE%D0%B9+%D1%81%D0%B5%D1%80%D0%B2%D0%B8%D1%81",
	} {
		got, err := ParseInvite(in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.URL != want.URL || got.Pin != want.Pin || (name != "plain" && name != "colons" && got.Name != want.Name) {
			t.Fatalf("%s: %+v", name, got)
		}
	}
	// Without a pin the usual certificate check applies.
	got, err := ParseInvite("https://sub.example.com/sub/TOKEN")
	if err != nil || got.Pin != "" || got.URL != "https://sub.example.com/sub/TOKEN" {
		t.Fatalf("no pin: %+v %v", got, err)
	}
	// The link made from an invite reads back the same.
	again, err := ParseInvite(want.Link())
	if err != nil || again != want {
		t.Fatalf("round trip: %+v %v", again, err)
	}
}

func TestParseInviteRefuses(t *testing.T) {
	for name, in := range map[string]string{
		"empty":        "",
		"profile link": share.Prefix + "abc",
		"no scheme":    "example.com/sub/x",
		"plain http":   "http://203.0.113.5/sub/x",
		"ftp":          "ftp://203.0.113.5/sub/x",
		"not a sub":    "https://example.com/page",
		"user info":    "https://user:pw@example.com/sub/x",
		"bad pin":      "https://example.com/sub/x#pin=1234",
		"non hex pin":  "https://example.com/sub/x#pin=" + strings.Repeat("zz", 32),
		"empty u":      "openflux://sub?p=" + pin,
	} {
		if inv, err := ParseInvite(in); err == nil {
			t.Errorf("%s: accepted %+v", name, inv)
		}
	}
	// Loopback http is for tests and a local server.
	if _, err := ParseInvite("http://127.0.0.1:9000/sub/x"); err != nil {
		t.Fatal(err)
	}
}

func TestLooksLikeInvite(t *testing.T) {
	for in, want := range map[string]bool{
		"openflux://sub?u=x":                  true,
		"https://h:8443/sub/TOKEN":            true,
		"  https://h/sub/T#pin=" + pin + "  ": true,
		share.Prefix + "abc":                  false,
		"https://example.com/page":            false,
		"hello":                               false,
		"":                                    false,
	} {
		if LooksLikeInvite(in) != want {
			t.Errorf("LooksLikeInvite(%q) = %v", in, !want)
		}
	}
}

func profileLink(t *testing.T, name string) string {
	t.Helper()
	r := share.Make(share.Config{Name: name, Negotiate: true, Secret: strings.Repeat("ab", 32),
		Transports: []share.Transport{{Type: "direct", Dial: "203.0.113.5:34443"}}})
	if r.Error != "" {
		t.Fatal(r.Error)
	}
	return r.Link
}

func TestDocumentCheck(t *testing.T) {
	ok := profileLink(t, "Германия")
	doc := Document{Version: 1, Title: "  Сервис  ", Status: StatusActive, UpdateHours: 0, Profiles: []Profile{
		{ID: "de", Name: "Германия", Link: ok},
		{ID: "de", Name: "Дубль", Link: ok},                 // the same id twice
		{ID: "bad id!", Name: "Плохой id", Link: ok},        // not a stable id
		{ID: "x", Name: "", Link: ok},                       // nothing to show
		{ID: "y", Name: "Мусор", Link: "openflux://v1/zzz"}, // not a link
		{ID: "z", Name: "Не наша", Link: "https://example.com"},
		{ID: "nl", Name: "Нидерланды", Link: ok},
	}}
	skipped, err := doc.Check()
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 5 || len(doc.Profiles) != 2 || doc.Profiles[0].ID != "de" || doc.Profiles[1].ID != "nl" {
		t.Fatalf("skipped %d: %+v", skipped, doc.Profiles)
	}
	if doc.Title != "Сервис" || doc.UpdateHours != 6 {
		t.Fatalf("defaults: %+v", doc)
	}

	for name, d := range map[string]Document{
		"version":     {Version: 2, Status: StatusActive, Profiles: []Profile{{ID: "a", Name: "A", Link: ok}}},
		"status":      {Version: 1, Status: "weird"},
		"no profiles": {Version: 1, Status: StatusActive},
		"all bad":     {Version: 1, Status: StatusActive, Profiles: []Profile{{ID: "a", Name: "A", Link: "nope"}}},
	} {
		if _, err := d.Check(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestInactiveDocumentHasNoProfiles(t *testing.T) {
	ok := profileLink(t, "A")
	for _, st := range []string{StatusExpired, StatusRevoked} {
		d := Document{Version: 1, Status: st, Profiles: []Profile{{ID: "a", Name: "A", Link: ok}}}
		if skipped, err := d.Check(); err != nil || skipped != 0 || len(d.Profiles) != 0 || d.Profiles == nil {
			t.Fatalf("%s: %d %v %+v", st, skipped, err, d.Profiles)
		}
	}
}

func TestResultJSON(t *testing.T) {
	var back Result
	if err := json.Unmarshal([]byte(Result{OK: true, Subscription: &Document{Version: 1, Status: StatusActive}, Skipped: 1}.JSON()), &back); err != nil || !back.OK || back.Skipped != 1 {
		t.Fatalf("%+v %v", back, err)
	}
}

func chunks(s string) []string {
	var out []string
	for i := 0; i < len(s); i += 2 {
		out = append(out, s[i:i+2])
	}
	return out
}
