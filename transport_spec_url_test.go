package main

import "testing"

// A per-type URL flag overrides a .conf transport's URL only when it is set:
// the unset flags are "", and overriding with them wiped the document URL of
// every [Transport] section, so a --config exit started vyandex with no URL.
func TestBuildTransportSpecsKeepsConfURL(t *testing.T) {
	specs := []transportSpec{
		{Name: "vyandex", Type: "vyandex", URL: "https://docs.yandex.ru/edit/d/conf"},
		{Name: "yandex", Type: "yandex", URL: "https://disk.yandex.ru/i/conf"},
	}
	urls := map[string]string{"vyandex": "", "yandex": "https://disk.yandex.ru/i/flag"}
	got := buildTransportSpecs(specs, urls, nil)
	if got[0].URL != "https://docs.yandex.ru/edit/d/conf" {
		t.Fatalf("unset flag wiped the conf URL: %q", got[0].URL)
	}
	if got[1].URL != "https://disk.yandex.ru/i/flag" {
		t.Fatalf("a set flag must override the conf URL: %q", got[1].URL)
	}
}
