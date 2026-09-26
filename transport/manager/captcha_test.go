package manager

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"openflux/transport"
	"openflux/transport/control"
)

// fakeNotifierTransport implements transport.ErrorNotifier and lets the test
// fire a captcha signal on demand.
type fakeNotifierTransport struct {
	fakeTransport
	notify atomic.Pointer[func(error, string, string, string)]
}

func (f *fakeNotifierTransport) SetErrorNotifier(fn func(err error, transportName, url, reason string)) {
	f.notify.Store(&fn)
}

func (f *fakeNotifierTransport) fireCaptcha(err error, name, url, reason string) {
	if p := f.notify.Load(); p != nil {
		(*p)(err, name, url, reason)
	}
}

func TestManagerCaptchaNotifier(t *testing.T) {
	sess, err := transport.NewSession(transport.PeerParameters{
		Capabilities:  control.CapabilityIPv4 | control.CapabilityTCP,
		MaxPacketSize: 1500,
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Stop()

	m := New(sess, nil, "test-secret-long-enough", "test-ctx")

	raw := &fakeNotifierTransport{}
	if err := m.Add("yandex", "yandex", raw, 100, nil); err != nil {
		t.Fatal(err)
	}

	got := make(chan struct {
		name   string
		url    string
		reason string
	}, 1)
	m.SetCaptchaNotifier(func(name, url, reason string) {
		got <- struct {
			name   string
			url    string
			reason string
		}{name, url, reason}
	})

	raw.fireCaptcha(errors.New("captcha"), "yandex", "https://doc", "smartcaptcha")

	select {
	case g := <-got:
		if g.name != "yandex" || g.url != "https://doc" || g.reason != "smartcaptcha" {
			t.Fatalf("got %+v", g)
		}
	case <-time.After(time.Second):
		t.Fatal("notifier not called")
	}
}
