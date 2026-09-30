package share

import "testing"

func streamCfg(typ, url string) Config {
	return Config{Name: "Free PHP node", Mode: ModeStream, Transports: []Transport{{Type: typ, URL: url}}}
}

// Stream mode rides in the link: the apps read it back and start the stream client.
func TestStreamModeRoundTrip(t *testing.T) {
	for _, c := range []struct{ typ, url string }{
		{"cupsonline", "https://interview.cups.online/live-coding/?room=11111111-2222-3333-4444-555555555555"},
		{"mailru", "https://cloud.mail.ru/public/Vuri/d5nuZ5aQp"},
	} {
		r := Make(streamCfg(c.typ, c.url))
		if r.Error != "" {
			t.Fatalf("%s: make: %s (%s)", c.typ, r.Error, r.Code)
		}
		back := Read(r.Link)
		if back.Error != "" || back.Config == nil {
			t.Fatalf("%s: read: %s (%s)", c.typ, back.Error, back.Code)
		}
		got := back.Config
		if got.Mode != ModeStream || len(got.Transports) != 1 || got.Transports[0].Type != c.typ || got.Transports[0].URL != c.url || got.Name != "Free PHP node" {
			t.Errorf("%s: read back %+v", c.typ, got)
		}
		if back.Context != "" {
			t.Errorf("%s: stream mode has no encryption context, got %q", c.typ, back.Context)
		}
	}
}

// The codes the apps word: what stream mode refuses, and why.
func TestStreamModeCodes(t *testing.T) {
	cases := []struct {
		name string
		c    Config
		code string
	}{
		{"unknown mode", Config{Mode: "teleport", Transports: []Transport{{Type: "mailru", URL: "x"}}}, CodeUnknownMode},
		{"two transports", Config{Mode: ModeStream, Transports: []Transport{{Type: "mailru", URL: "x"}, {Type: "cupsonline"}}}, CodeStreamOneTransport},
		{"no transport", Config{Mode: ModeStream}, CodeNoTransports},
		{"a carrier the PHP exit has no port for", streamCfg("boards", "https://b"), CodeStreamTransport},
		{"yandex has none either", streamCfg("yandex", "https://docs.yandex.ru/x"), CodeStreamTransport},
		{"direct", Config{Mode: ModeStream, Transports: []Transport{{Type: "direct", Dial: "1.2.3.4:5"}}}, CodeStreamTransport},
		{"a session", Config{Mode: ModeStream, Negotiate: true, Secret: "0123456789abcdef", Transports: []Transport{{Type: "mailru", URL: "x"}}}, CodeStreamPlainOnly},
		{"a secret", Config{Mode: ModeStream, Secret: "0123456789abcdef", Transports: []Transport{{Type: "mailru", URL: "x"}}}, CodeStreamPlainOnly},
	}
	for _, c := range cases {
		if r := Make(c.c); r.Code != c.code {
			t.Errorf("%s: code %q, want %q (%s)", c.name, r.Code, c.code, r.Error)
		}
	}
}

// Nothing changes for links without a mode: the field is absent from what
// earlier builds wrote and from what this one writes.
func TestClassicLinksCarryNoMode(t *testing.T) {
	r := Make(Config{Transports: []Transport{{Type: "cupsonline", URL: "https://interview.cups.online/live-coding/?room=a"}}})
	if r.Error != "" {
		t.Fatal(r.Error)
	}
	if r.Config.Mode != "" {
		t.Errorf("a classic link got mode %q", r.Config.Mode)
	}
	if back := Read(r.Link); back.Config == nil || back.Config.Mode != "" {
		t.Errorf("read back %+v", back)
	}
}
