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
