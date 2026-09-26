package manager

import (
	"path/filepath"
	"testing"

	"openflux/transport"
	"openflux/transport/control"
)

// An exit signed in with its owner's Yandex account must not hand that
// login to clients: everyone with the channel key can ask for the jar.
func TestShareableCookiesDropAccountLogin(t *testing.T) {
	jar := map[string]string{
		"Session_id": "s", "sessionid2": "s2", "sessar": "a", "L": "l",
		"yandex_login": "me", "spravka": "captcha-pass", "yandexuid": "u",
	}
	got := shareableCookies(jar)
	for _, name := range []string{"Session_id", "sessionid2", "sessar", "L", "yandex_login"} {
		if _, ok := got[name]; ok {
			t.Fatalf("%s must not be shared: %v", name, got)
		}
	}
	if got["spravka"] != "captcha-pass" || got["yandexuid"] != "u" {
		t.Fatalf("non-account cookies must still be shared: %v", got)
	}
	if jar["Session_id"] != "s" {
		t.Fatal("the source jar must not be modified")
	}
}

// Cookies a client sends after passing a check add to what the exit keeps
// (its account login) instead of replacing it.
func TestAcceptCookiesMergesWithStored(t *testing.T) {
	m := newTestManager(t)
	p := &fakeCookieProvider{}
	if err := m.Add("vyandex", "vyandex", &fakeTransport{}, 100, p); err != nil {
		t.Fatal(err)
	}
	store, err := transport.NewCookieStore(filepath.Join(t.TempDir(), "cookies.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save("doc", map[string]string{"Session_id": "login", "spravka": "old"}); err != nil {
		t.Fatal(err)
	}
	if err := m.UseCookieStore(store, "vyandex", "doc"); err != nil {
		t.Fatal(err)
	}
	m.DispatchControl(control.SubtypeCookiesOffer, offer(t, "vyandex", map[string]string{"spravka": "new"}))
	jar, _ := p.FetchCookies()
	if jar["Session_id"] != "login" || jar["spravka"] != "new" {
		t.Fatalf("live jar = %v", jar)
	}
	if saved := store.Load("doc"); saved["Session_id"] != "login" || saved["spravka"] != "new" {
		t.Fatalf("stored jar = %v", saved)
	}
}
