package provision

import (
	"strings"
	"testing"

	"github.com/p1neappleXpress/OpenFlux/share"
)

const (
	testVolga  = "https://docs.yandex.ru/edit/d/AbCdEfGhIjKlMnOpQrStUv"
	testMailru = "https://cloud.mail.ru/public/DEmN/ETbZW2MPY"
	testRooms  = "WyJyb29tLTEiLCJyb29tLTIiXQ"
)

func TestCheckTransportsCleansAndOrders(t *testing.T) {
	got, err := CheckTransports([]ChannelTransport{
		{Type: "cupsonline", URL: testRooms},
		{Type: "mailru", URL: " " + testMailru + "/?weblink=x "},
		{Type: "vyandex", URL: testVolga + "#section"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []ChannelTransport{{"vyandex", testVolga}, {"mailru", testMailru}, {"cupsonline", testRooms}}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	}
	if got, err := CheckTransports(nil); err != nil || len(got) != 0 {
		t.Fatalf("direct only: %v %v", got, err)
	}
}

func TestCheckTransportsRefuses(t *testing.T) {
	for name, ts := range map[string][]ChannelTransport{
		"twice":        {{"vyandex", testVolga}, {"vyandex", testVolga}},
		"unknown":      {{"boards", "https://boards.yandex.ru/whiteboard/?hash=1"}},
		"direct":       {{"direct", "1.2.3.4:5"}},
		"max":          {{"oneme", "token"}},
		"volga link":   {{"vyandex", "https://disk.yandex.ru/i/abc"}},
		"mailru link":  {{"mailru", "https://cloud.mail.ru/home/doc.docx"}},
		"mailru other": {{"mailru", "https://evil.example/public/a/b"}},
		"rooms":        {{"cupsonline", "not base64!"}},
		"no rooms":     {{"cupsonline", ""}},
		"newline":      {{"mailru", testMailru + "\nkey=0"}},
	} {
		if _, err := CheckTransports(ts); err == nil {
			t.Errorf("%s: accepted %+v", name, ts)
		}
	}
}

func TestSessionContextPicksTheTopDocument(t *testing.T) {
	for _, c := range []struct {
		ts   []ChannelTransport
		want string
	}{
		{[]ChannelTransport{{"vyandex", testVolga}, {"mailru", testMailru}}, testVolga},
		{[]ChannelTransport{{"cupsonline", testRooms}, {"mailru", testMailru}}, testMailru},
		{[]ChannelTransport{{"cupsonline", testRooms}}, "http://#"},
		{nil, "http://#"},
	} {
		if got := SessionContext(c.ts); got != c.want {
			t.Errorf("SessionContext(%+v) = %q, want %q", c.ts, got, c.want)
		}
	}
}

func TestShareLinkCarriesTheChosenTransports(t *testing.T) {
	key := strings.Repeat("ab", 32)
	link, err := ShareLink("Нода", key, "203.0.113.5", 8445, []ChannelTransport{{"cupsonline", testRooms}, {"mailru", testMailru}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := share.Decode(link)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Negotiate || c.Secret != key || c.Context != testMailru || c.Name != "Нода" {
		t.Fatalf("link: %+v", c)
	}
	want := []share.Transport{
		{Type: "mailru", URL: testMailru, Priority: 90},
		{Type: "cupsonline", URL: testRooms, Priority: 70},
		{Type: "direct", Dial: "203.0.113.5:8445", Priority: 50},
	}
	if len(c.Transports) != len(want) {
		t.Fatalf("transports: %+v", c.Transports)
	}
	for i := range want {
		if c.Transports[i] != want[i] {
			t.Fatalf("transport %d: %+v, want %+v", i, c.Transports[i], want[i])
		}
	}

	link, err = ShareLink("Только direct", key, "2001:db8::1", 8445, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c, _ = share.Decode(link); len(c.Transports) != 1 || c.Transports[0].Dial != "[2001:db8::1]:8445" || c.Context != "http://#" {
		t.Fatalf("direct only: %+v", c)
	}
	if _, err := ShareLink("x", key, "", 8445, nil); err == nil {
		t.Fatal("no host must fail")
	}
}

func TestChannelConfigLines(t *testing.T) {
	ch := Channel{ID: "of-abc", Transports: []ChannelTransport{{"mailru", testMailru}, {"vyandex", testVolga}}, AutoUpdate: true}
	cfg, err := ch.config()
	if err != nil {
		t.Fatal(err)
	}
	want := "channel=of-abc\nvyandex=" + testVolga + "\nmailru=" + testMailru + "\nautoupdate=yes\n"
	if cfg != want {
		t.Fatalf("config:\n%s\nwant:\n%s", cfg, want)
	}
	ch.Port, ch.AutoUpdate, ch.Transports = 20001, false, nil
	if cfg, _ = ch.config(); cfg != "channel=of-abc\nport=20001\nautoupdate=no\n" {
		t.Fatalf("direct only config:\n%s", cfg)
	}
	ch.Transports = []ChannelTransport{{"mailru", testMailru + "\nkey=" + strings.Repeat("0", 64)}}
	if _, err := ch.config(); err == nil {
		t.Fatal("a line break in a link must not reach the script")
	}
}
