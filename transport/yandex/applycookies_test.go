package yandex

import (
	"net/url"
	"testing"

	"openflux/transport"
)

// Cookies handed in from an out-of-band solve must reach every Yandex host
// the document fetch is redirected through, not just the link's own host.
func TestApplyCookiesCoversRedirectHosts(t *testing.T) {
	tr := NewYandexDocsTransport("https://disk.yandex.ru/i/abc", transport.DefaultConfig())
	if err := tr.ApplyCookies(map[string]string{"spravka": "s1"}); err != nil {
		t.Fatal(err)
	}
	docs, _ := url.Parse("https://docs.yandex.ru/docs/view?url=x")
	found := false
	for _, c := range tr.cookieJar.Cookies(docs) {
		if c.Name == "spravka" && c.Value == "s1" {
			found = true
		}
	}
	if !found {
		t.Fatal("spravka cookie not sent to docs.yandex.ru")
	}
}
