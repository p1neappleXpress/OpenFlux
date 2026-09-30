package share

import (
	"bytes"
	"compress/flate"
	"encoding/base64"
	"strings"
	"testing"
)

func sample() Config {
	return Config{
		Name:      "VDS",
		Negotiate: true,
		Secret:    "a shared secret of 32 characters",
		Context:   "https://disk.yandex.ru/i/abc",
		Transports: []Transport{
			{Type: "direct", Priority: 100, Dial: "203.0.113.7:8445"},
			{Type: "yandex", Priority: 50, URL: "https://disk.yandex.ru/i/abc"},
		},
	}
}

func TestRoundTrip(t *testing.T) {
	link, err := Encode(sample())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(link, Prefix) {
		t.Fatalf("link %q lacks %q", link, Prefix)
	}
	// Scannable into a URL field or a messenger: no characters that need
	// escaping.
	for _, r := range strings.TrimPrefix(link, Prefix) {
		if !strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_", r) {
			t.Fatalf("link contains %q", r)
		}
	}
	got, err := Decode("  " + link + "\n")
	if err != nil {
		t.Fatal(err)
	}
	want := sample()
	if got.Name != want.Name || got.Secret != want.Secret || got.Context != want.Context ||
		!got.Negotiate || len(got.Transports) != 2 || got.Transports[0] != want.Transports[0] ||
		got.Transports[1] != want.Transports[1] {
		t.Fatalf("round trip changed the config: %+v", got)
	}
}

func TestValidateRejects(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"no transports":             func(c *Config) { c.Transports = nil },
		"several without a session": func(c *Config) { c.Negotiate = false },
		"short secret":              func(c *Config) { c.Secret = "short" },
		"unknown type":              func(c *Config) { c.Transports[1].Type = "carrier-pigeon" },
		"MAX token":                 func(c *Config) { c.Transports[1].Type = "oneme" },
		"direct without address":    func(c *Config) { c.Transports[0].Dial = "" },
		"unknown codec":             func(c *Config) { c.Codec = "gzip" },
	} {
		c := sample()
		mutate(&c)
		if _, err := Encode(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestDecodeRejects(t *testing.T) {
	bomb := bytes.Repeat([]byte("A"), maxPayload*4)
	for name, link := range map[string]string{
		"other scheme":   "https://example.com",
		"other version":  "openflux://v9/abc",
		"bad base64":     Prefix + "***",
		"not deflate":    Prefix + base64.RawURLEncoding.EncodeToString([]byte("plain text")),
		"oversized":      Prefix + deflated(t, bomb),
		"not json":       Prefix + deflated(t, []byte("not json")),
		"invalid config": Prefix + deflated(t, []byte(`{"transports":[]}`)),
	} {
		if _, err := Decode(link); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestQRRenderings(t *testing.T) {
	link, err := Encode(sample())
	if err != nil {
		t.Fatal(err)
	}
	bitmap, err := Bitmap(link)
	if err != nil {
		t.Fatal(err)
	}
	if len(bitmap) < 21 || len(bitmap[0]) != len(bitmap) {
		t.Fatalf("bitmap is %dx%d", len(bitmap), len(bitmap[0]))
	}
	png, err := PNG(link, 512)
	if err != nil || !bytes.HasPrefix(png, []byte("\x89PNG\r\n\x1a\n")) {
		t.Fatalf("PNG: %v", err)
	}
	text, err := Terminal(link)
	if err != nil || strings.Count(text, "\n") < 10 {
		t.Fatalf("terminal rendering: %v", err)
	}
}

func deflated(t *testing.T, raw []byte) string {
	t.Helper()
	var buf bytes.Buffer
	w, _ := flate.NewWriter(&buf, flate.BestSpeed)
	_, _ = w.Write(raw)
	_ = w.Close()
	return base64.RawURLEncoding.EncodeToString(buf.Bytes())
}

// Links reach Decode after copying, chats and other encoders; every client
// must read them the same way.
func TestDecodeTolerant(t *testing.T) {
	c := Config{Name: "tt", Secret: "0123456789abcdef0123", Context: "https://cloud.mail.ru/public/AbCd/EfGh",
		Transports: []Transport{{Type: "mailru", URL: "https://cloud.mail.ru/public/AbCd/EfGh"}}}
	var link string
	for {
		l, err := Encode(c)
		if err != nil {
			t.Fatal(err)
		}
		if len(strings.TrimPrefix(l, Prefix))%4 != 0 {
			link = l
			break
		}
		c.Name += "x"
	}
	body := strings.TrimPrefix(link, Prefix)
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		t.Fatal(err)
	}
	variants := map[string]string{
		"padded":       Prefix + base64.URLEncoding.EncodeToString(raw),
		"wrapped":      link[:40] + "\n" + link[40:80] + "\r\n  " + link[80:],
		"std alphabet": Prefix + base64.RawStdEncoding.EncodeToString(raw),
		"nbsp":         link[:50] + " " + link[50:],
	}
	for name, v := range variants {
		got, err := Decode(v)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got.Name != c.Name {
			t.Errorf("%s: name %q", name, got.Name)
		}
	}
	if _, err := Decode(strings.ToUpper(link)); err == nil || !strings.Contains(err.Error(), "case") {
		t.Errorf("upper-cased link: %v", err)
	}
}

// The secret is counted in characters as Kotlin counts them, not bytes.
func TestSecretCountedInCharacters(t *testing.T) {
	c := Config{Secret: "ключключкл", Transports: []Transport{{Type: "mailru", URL: "https://cloud.mail.ru/public/a/b"}}}
	if err := c.Validate(); err == nil {
		t.Fatal("10 Cyrillic letters (20 bytes) accepted as a 16-character secret")
	}
	c.Secret = "ключключключключ"
	if err := c.Validate(); err != nil {
		t.Fatalf("16 Cyrillic letters: %v", err)
	}
}
