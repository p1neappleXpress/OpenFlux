package mobile

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport"
)

type fakeCaptchaTransport struct {
	transport.Transport
	notify  func(err error, name, url, reason string)
	applied chan map[string]string
}

func (f *fakeCaptchaTransport) SetErrorNotifier(fn func(err error, name, url, reason string)) {
	f.notify = fn
}
func (f *fakeCaptchaTransport) FetchCookies() (map[string]string, error) { return nil, nil }
func (f *fakeCaptchaTransport) ApplyCookies(jar map[string]string) error {
	f.applied <- jar
	return nil
}

func TestCaptchaFlow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.json")
	if msg := SetCookieStorePath(path); msg != "" {
		t.Fatal(msg)
	}
	raw := &fakeCaptchaTransport{applied: make(chan map[string]string, 1)}
	attachCaptcha("yandex", "https://docs.example/d", raw)

	raw.notify(errors.New("captcha"), "yandex", "https://docs.example/d", "smartcaptcha")
	if PendingCaptchaURL() != "https://docs.example/d" || PendingCaptchaReason() != "smartcaptcha" {
		t.Fatalf("pending = %q/%q", PendingCaptchaURL(), PendingCaptchaReason())
	}

	if msg := SubmitCaptchaCookies("spravka=abc; yandexuid=42;bad; =x"); msg != "" {
		t.Fatal(msg)
	}
	if PendingCaptchaURL() != "" {
		t.Fatal("pending not cleared after submit")
	}
	select {
	case jar := <-raw.applied:
		if len(jar) != 2 || jar["spravka"] != "abc" || jar["yandexuid"] != "42" {
			t.Fatalf("applied jar = %v", jar)
		}
	case <-time.After(time.Second):
		t.Fatal("cookies not applied to live transport")
	}

	// A fresh start with the same transport/URL replays the saved cookies.
	next := &fakeCaptchaTransport{applied: make(chan map[string]string, 1)}
	SetCookieStorePath(path)
	attachCaptcha("yandex", "https://docs.example/d", next)
	select {
	case jar := <-next.applied:
		if jar["spravka"] != "abc" {
			t.Fatalf("replayed jar = %v", jar)
		}
	default:
		t.Fatal("saved cookies not replayed on next start")
	}

	// After detach (stopped / failed start) cookies are only saved.
	detachCaptcha()
	SubmitCaptchaCookies("spravka=new")
	select {
	case jar := <-next.applied:
		t.Fatalf("applied to detached transport: %v", jar)
	case <-time.After(50 * time.Millisecond):
	}
}
