package main

import (
	"strings"
	"testing"

	"openflux/share"
)

// The link an exit prints must decode to what a client needs: the exit's
// transports, key and context, with direct dialing the shared host.
func TestShareConfigFromExitSpecs(t *testing.T) {
	specs := []transportSpec{
		{Name: "direct", Type: "direct", Priority: 100, Params: map[string]interface{}{"listen": "0.0.0.0:8445", "is_exit": true}},
		{Name: "yandex", Type: "yandex", Priority: 50, URL: "https://disk.yandex.ru/i/abc"},
		{Name: "oneme", Type: "oneme", Priority: 10},
	}
	c, skipped := shareConfig(specs, true, codecBatched, "a shared secret of 32 characters", "https://disk.yandex.ru/i/abc", "203.0.113.7")
	if len(skipped) != 1 || !strings.HasPrefix(skipped[0], "oneme") {
		t.Fatalf("skipped = %v, want the MAX transport only", skipped)
	}
	link, err := share.Encode(c)
	if err != nil {
		t.Fatal(err)
	}
	got, err := share.Decode(link)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Negotiate || got.Context != "https://disk.yandex.ru/i/abc" || len(got.Transports) != 2 ||
		got.Transports[0].Dial != "203.0.113.7:8445" || got.Transports[1].URL != "https://disk.yandex.ru/i/abc" {
		t.Fatalf("decoded %+v", got)
	}

	_, skipped = shareConfig(specs[:1], true, codecBatched, "a shared secret of 32 characters", "", "")
	if len(skipped) != 1 {
		t.Fatal("direct without a host to share must be left out")
	}
}
