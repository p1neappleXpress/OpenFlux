package yandex

import (
	"bytes"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport"
	"github.com/p1neappleXpress/OpenFlux/utils"
)

// The redirect target is synthetic. Neither transport may fetch it: the
// browser needs the original challenge, including its opaque query.
func TestAuthRedirectReachesNotifier(t *testing.T) {
	for _, kind := range []string{"vyandex", "yandex"} {
		t.Run(kind, func(t *testing.T) {
			for _, tc := range []struct {
				name, location, reason string
				cause                  error
			}{
				{"captcha", "https://captcha.example/showcaptcha?synthetic_token=must-stay-private#fragment", "smartcaptcha", ErrCaptchaRequired},
				{"relative_root", "/showcaptcha?synthetic_token=must-stay-private", "smartcaptcha", ErrCaptchaRequired},
				{"relative_path", "../showcaptcha?synthetic_token=must-stay-private", "smartcaptcha", ErrCaptchaRequired},
				{"login", "https://passport.yandex.ru/auth?synthetic_token=must-stay-private", "login", ErrLoginRequired},
				{"relative_login", "/passport.yandex/auth?synthetic_token=must-stay-private", "login", ErrLoginRequired},
			} {
				t.Run(tc.name, func(t *testing.T) {
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path == "/document" {
							http.Redirect(w, r, "http://"+r.Host+"/nested/document", http.StatusFound)
							return
						}
						http.Redirect(w, r, tc.location, http.StatusFound)
					}))
					defer srv.Close()
					doc := srv.URL + "/document"
					base, _ := url.Parse(srv.URL + "/nested/document")
					ref, _ := url.Parse(tc.location)
					challenge := base.ResolveReference(ref).String()
					type notification struct {
						err                     error
						name, url, html, reason string
					}
					got := make(chan notification, 1)
					notify := func(err error, name, url, html, reason string) { got <- notification{err, name, url, html, reason} }
					if kind == "vyandex" {
						tr := NewYandexVolgaTransport(doc, transport.DefaultConfig())
						tr.SetErrorNotifier(notify)
						defer tr.Stop()
						if err := tr.Start(); !errors.Is(err, tc.cause) {
							t.Fatal("Start lost auth sentinel")
						}
					} else {
						tr := NewYandexDocsTransport(doc, transport.DefaultConfig())
						tr.SetErrorNotifier(notify)
						defer tr.Stop()
						if err := tr.Start(); err != nil {
							t.Fatal(err)
						}
					}
					select {
					case n := <-got:
						if !errors.Is(n.err, tc.cause) {
							t.Fatal("notifier lost auth sentinel")
						}
						if n.name != kind || n.reason != tc.reason || n.html != "" {
							t.Fatal("incorrect notifier type/reason")
						}
						if n.url != challenge {
							t.Fatalf("challenge URL lost: notifier_uses_original_document=%t; errors.Is(ErrCaptchaRequired)=true", n.url == doc)
						}
					case <-time.After(3 * time.Second):
						t.Fatal("notifier not called")
					}
				})
			}
		})
	}
}

func TestAuthRedirectDebugLogsArePrivate(t *testing.T) {
	var mu sync.Mutex
	var logs []string
	oldLevel := utils.Level()
	utils.SetLevel(utils.LevelDebug)
	utils.SetLogSink(func(s string) { mu.Lock(); logs = append(logs, s); mu.Unlock() })
	var output bytes.Buffer
	oldOutput := log.Writer()
	log.SetOutput(&output)
	defer func() { utils.SetLogSink(nil); utils.SetLevel(oldLevel); log.SetOutput(oldOutput) }()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://captcha.example/showcaptcha?must-stay-private#private-fragment", http.StatusFound)
	}))
	defer srv.Close()
	_, _ = authorizeWithJar(srv.URL, nil)
	tr := NewYandexDocsTransport(srv.URL, transport.DefaultConfig())
	_, _ = tr.fetchDocInfo(srv.URL, "synthetic-user")
	mu.Lock()
	defer mu.Unlock()
	for _, line := range append(logs, output.String()) {
		if strings.Contains(line, "must-stay-private") || strings.Contains(line, "private-fragment") {
			t.Fatal("redirect token leaked to logs")
		}
	}
}
