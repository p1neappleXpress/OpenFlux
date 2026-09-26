package yandex

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"openflux/transport"
)

// captchaDoc serves a document that always redirects to SmartCaptcha and
// reports each fetch on hits.
func captchaDoc(t *testing.T) (url string, hits chan time.Time) {
	t.Helper()
	hits = make(chan time.Time, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/doc" {
			hits <- time.Now()
			http.Redirect(w, r, "/showcaptcha?cc=1", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/doc", hits
}

func waitHit(t *testing.T, hits chan time.Time, within time.Duration) time.Time {
	t.Helper()
	select {
	case at := <-hits:
		return at
	case <-time.After(within):
		t.Fatalf("no document fetch within %v", within)
		return time.Time{}
	}
}

func captchaWaiters() int {
	buf := make([]byte, 1<<20)
	return strings.Count(string(buf[:runtime.Stack(buf, true)]), "scheduleReconnectNoCaptcha")
}

func startCaptchaTransport(t *testing.T, url string) (*YandexDocsTransport, *atomic.Int32) {
	t.Helper()
	tr := NewYandexDocsTransport(url, transport.DefaultConfig())
	var notified atomic.Int32
	tr.SetErrorNotifier(func(error, string, string, string) { notified.Add(1) })
	if err := tr.Start(); err != nil {
		t.Fatal(err)
	}
	return tr, &notified
}

func TestStopInterruptsCaptchaWait(t *testing.T) {
	url, hits := captchaDoc(t)
	tr, notified := startCaptchaTransport(t, url)
	waitHit(t, hits, 5*time.Second)
	deadline := time.Now().Add(2 * time.Second)
	for captchaWaiters() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if notified.Load() == 0 || captchaWaiters() == 0 {
		t.Fatal("transport did not enter the captcha wait")
	}

	_ = tr.Stop()
	deadline = time.Now().Add(2 * time.Second)
	for captchaWaiters() > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := captchaWaiters(); n > 0 {
		t.Fatalf("captcha wait still running after Stop (%d goroutines)", n)
	}
}

// Cookies arriving during the captcha wait must end that wait and retry
// right away, instead of scheduling a second reconnect next to it (which
// opened a duplicate session to the document once the wait expired).
func TestApplyCookiesWakesCaptchaWait(t *testing.T) {
	url, hits := captchaDoc(t)
	tr, _ := startCaptchaTransport(t, url)
	defer tr.Stop()
	waitHit(t, hits, 5*time.Second)
	deadline := time.Now().Add(2 * time.Second)
	for captchaWaiters() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	start := time.Now()
	if err := tr.ApplyCookies(map[string]string{"spravka": "s1"}); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Fatalf("ApplyCookies blocked for %v on its own reconnect", took)
	}
	if at := waitHit(t, hits, 5*time.Second); at.Sub(start) > time.Second {
		t.Fatalf("retry came %v after ApplyCookies, want an immediate wake", at.Sub(start))
	}
}
