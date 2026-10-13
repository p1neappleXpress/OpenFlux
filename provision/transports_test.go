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
	want := []ChannelTransport{{Type: "vyandex", URL: testVolga}, {Type: "mailru", URL: testMailru}, {Type: "cupsonline", URL: testRooms}}
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
		"twice":        {{Type: "vyandex", URL: testVolga}, {Type: "vyandex", URL: testVolga}},
		"board hash":   {{Type: "boards", URL: "https://boards.yandex.ru/whiteboard/?hash=1"}},
		"direct":       {{Type: "direct", URL: "1.2.3.4:5"}},
		"max":          {{Type: "oneme", URL: "token"}},
		"volga link":   {{Type: "vyandex", URL: "https://disk.yandex.ru/i/abc"}},
		"mailru link":  {{Type: "mailru", URL: "https://cloud.mail.ru/home/doc.docx"}},
		"mailru other": {{Type: "mailru", URL: "https://evil.example/public/a/b"}},
		"board host":   {{Type: "boards", URL: "https://evil.example/whiteboard/?hash=820d6571111dffeb9e85de65ccfc880a"}},
		"yandex other": {{Type: "yandex", URL: "https://evil.example/edit/d/AbCdEfGhIjKlMnOpQrStUv"}},
		"rooms":        {{Type: "cupsonline", URL: "not base64!"}},
		"no rooms":     {{Type: "cupsonline", URL: ""}},
		"newline":      {{Type: "mailru", URL: testMailru + "\nkey=0"}},
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
		{[]ChannelTransport{{Type: "vyandex", URL: testVolga}, {Type: "mailru", URL: testMailru}}, testVolga},
		{[]ChannelTransport{{Type: "cupsonline", URL: testRooms}, {Type: "mailru", URL: testMailru}}, testMailru},
		{[]ChannelTransport{{Type: "cupsonline", URL: testRooms}}, "http://#"},
		{nil, "http://#"},
	} {
		if got := SessionContext(c.ts); got != c.want {
			t.Errorf("SessionContext(%+v) = %q, want %q", c.ts, got, c.want)
		}
	}
}

func TestShareLinkCarriesTheChosenTransports(t *testing.T) {
	key := strings.Repeat("ab", 32)
	link, err := ShareLink("Нода", key, "203.0.113.5", 8445, []ChannelTransport{{Type: "cupsonline", URL: testRooms}, {Type: "mailru", URL: testMailru}})
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
	ch := Channel{ID: "of-abc", Transports: []ChannelTransport{{Type: "mailru", URL: testMailru}, {Type: "vyandex", URL: testVolga}}, AutoUpdate: true}
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
	ch.Transports = []ChannelTransport{{Type: "mailru", URL: testMailru + "\nkey=" + strings.Repeat("0", 64)}}
	if _, err := ch.config(); err == nil {
		t.Fatal("a line break in a link must not reach the script")
	}
}

func TestPinnedForSources(t *testing.T) {
	fork, err := PinnedFor("")
	if err != nil || fork != Pinned() || !strings.Contains(fork.URL, "/"+PinnedRepo+"/") {
		t.Fatalf("default: %+v %v", fork, err)
	}
	official, err := PinnedFor(SourceOfficial)
	if err != nil || !strings.Contains(official.URL, "/p1neappleXpress/OpenFlux/"+OfficialCommit+"/") || official.SHA256 != OfficialSHA256 {
		t.Fatalf("official: %+v %v", official, err)
	}
	if _, err := PinnedFor("elsewhere"); err == nil {
		t.Fatal("an unknown source must fail")
	}
}

func TestCheckTransportsYandexDocsAndBoards(t *testing.T) {
	board := "https://boards.yandex.ru/whiteboard/?hash=820d6571111dffeb9e85de65ccfc880a"
	got, err := CheckTransports([]ChannelTransport{
		{Type: "boards", URL: board + "&from=cabinet#x"},
		{Type: "cupsonline", URL: testRooms},
		{Type: "mailru", URL: testMailru},
		{Type: "yandex", URL: testVolga + "?sk=y"},
		{Type: "vyandex", URL: testVolga},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []ChannelTransport{{Type: "vyandex", URL: testVolga}, {Type: "yandex", URL: testVolga}, {Type: "mailru", URL: testMailru}, {Type: "boards", URL: board}, {Type: "cupsonline", URL: testRooms}}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	}
	if _, err := CheckTransports([]ChannelTransport{{Type: "yandex", URL: "https://disk.yandex.ru/i/AbCdEf123"}}); err != nil {
		t.Fatalf("public disk link: %v", err)
	}
	// A channel of a board alone derives its context from the board.
	if c := SessionContext([]ChannelTransport{{Type: "boards", URL: board}}); c != board {
		t.Fatalf("board context %q", c)
	}
}

// A channel whose documents changed keeps the context it was made with: the
// link says that one, not the one its new first document would give.
func TestShareLinkContextKeepsTheChannelContext(t *testing.T) {
	key := strings.Repeat("cd", 32)
	board := "https://boards.yandex.ru/whiteboard/?hash=0123456789abcdef0123456789abcdef"
	ts := []ChannelTransport{{Type: "boards", URL: board}, {Type: "cupsonline", URL: testRooms}}
	link, err := ShareLinkContext("Нода", key, "203.0.113.5", 8445, ts, testMailru)
	if err != nil {
		t.Fatal(err)
	}
	c, err := share.Decode(link)
	if err != nil {
		t.Fatal(err)
	}
	if c.Context != testMailru {
		t.Fatalf("context %q, want the channel's %q", c.Context, testMailru)
	}
	if c.Transports[0].Type != "boards" || c.Transports[0].URL != board {
		t.Fatalf("transports: %+v", c.Transports)
	}
	// No context given: derived from the documents, as for a new channel.
	link, err = ShareLinkContext("Нода", key, "203.0.113.5", 8445, ts, " ")
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := share.Decode(link); c.Context != SessionContext(ts) || c.Context == testMailru {
		t.Fatalf("derived context %q, want %q", c.Context, SessionContext(ts))
	}
}

// SetTransports checks the documents before it touches the server.
func TestSetTransportsChecksBeforeTheServer(t *testing.T) {
	c := &Conn{}
	if err := c.SetTransports("of-abc123", nil, "", ""); err == nil || !strings.Contains(err.Error(), "хотя бы один документ") {
		t.Fatalf("no documents: %v", err)
	}
	if err := c.SetTransports("of-abc123", []ChannelTransport{{Type: "mailru", URL: "https://example.com/x"}}, "", ""); err == nil || !strings.Contains(err.Error(), "Mail.ru") {
		t.Fatalf("bad link: %v", err)
	}
	if err := c.SetTransports("of-abc123", []ChannelTransport{{Type: "direct", URL: "x"}}, "", ""); err == nil {
		t.Fatal("direct is always there and cannot be chosen")
	}
	// Valid documents reach the script, which this connection has not fetched.
	if err := c.SetTransports("of-abc123", []ChannelTransport{{Type: "mailru", URL: testMailru}}, "", ""); err == nil || !strings.Contains(err.Error(), "скрипт установки не загружен") {
		t.Fatalf("valid documents: %v", err)
	}
}

// A Telemost conference link is kept without its query; anything else is not one.
func TestCheckTransportsTelemost(t *testing.T) {
	const room = "https://telemost.yandex.ru/j/88936358186517"
	for _, in := range []string{room, room + "/", room + "?utm=x", " " + room + "#a "} {
		got, err := CheckTransports([]ChannelTransport{{Type: "telemost", URL: in}})
		if err != nil || got[0].URL != room {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	for _, in := range []string{"https://telemost.yandex.ru/j/123abc", "https://telemost.yandex.ru/j/12", "https://example.com/j/88936358186517", "http://telemost.yandex.ru/j/88936358186517", ""} {
		if _, err := CheckTransports([]ChannelTransport{{Type: "telemost", URL: in}}); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestChannelMode(t *testing.T) {
	for in, want := range map[string]string{"": "l4", "l4": "l4", " L3 ": "l3", "l3": "l3"} {
		got, err := CheckMode(in)
		if err != nil || got != want {
			t.Errorf("CheckMode(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := CheckMode("l2"); err == nil {
		t.Error("an unknown mode is refused")
	}
	// l4 is the script's default: nothing is sent for it; l3 is named.
	ch := Channel{ID: "of-abc", Port: 20001}
	if cfg, _ := ch.config(); strings.Contains(cfg, "mode=") {
		t.Errorf("default config names a mode: %q", cfg)
	}
	ch.Mode = "l3"
	if cfg, _ := ch.config(); !strings.Contains(cfg, "mode=l3") {
		t.Errorf("l3 config lacks the mode: %q", cfg)
	}
	ch.Mode = "bogus"
	if _, err := ch.config(); err == nil {
		t.Error("a bad mode is refused before it reaches the server")
	}
}
