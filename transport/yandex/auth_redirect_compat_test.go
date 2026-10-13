package yandex

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/p1neappleXpress/OpenFlux/transport"
)

// Ordinary redirects and the first-tier PoW flow must still reach the document,
// rather than being reported as an out-of-band SmartCaptcha challenge.
func TestAuthRedirectPreservesOrdinaryAndPoWPaths(t *testing.T) {
	for _, kind := range []string{"vyandex", "yandex"} {
		for _, pow := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/pow=%t", kind, pow), func(t *testing.T) {
				var submitted, reachedDocument atomic.Bool
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					origin := "http://" + r.Host
					switch r.URL.Path {
					case "/start":
						if pow && !submitted.Load() {
							http.Redirect(w, r, origin+"/showcaptchafast", http.StatusFound)
						} else {
							http.Redirect(w, r, origin+"/document", http.StatusFound)
						}
					case "/showcaptchafast":
						data := base64.StdEncoding.EncodeToString([]byte(`{"uniqueKey":"synthetic","pow":{"complexity":0,"prefix":"test"},"timestamp":1}`))
						fmt.Fprintf(w, `<script>window.__SSR_DATA__ = JSON.parse(atob("%s"))</script><form id="tmgrdfrend-form" action="%s/answer"></form>`, data, origin)
					case "/answer":
						if r.Method != http.MethodPost {
							t.Error("expected PoW POST")
						}
						submitted.Store(true)
						http.Redirect(w, r, origin+"/start", http.StatusFound)
					case "/document":
						reachedDocument.Store(true)
						// Deliberately no editor configuration: stop before any real service IO.
						fmt.Fprint(w, "<html>synthetic document</html>")
					default:
						t.Error("unexpected request")
						http.NotFound(w, r)
					}
				}))
				defer srv.Close()
				var err error
				if kind == "vyandex" {
					_, err = authorizeWithJar(srv.URL+"/start", nil)
				} else {
					tr := NewYandexDocsTransport(srv.URL+"/start", transport.DefaultConfig())
					_, err = tr.fetchDocInfo(srv.URL+"/start", "synthetic-user")
				}
				if !reachedDocument.Load() || submitted.Load() != pow {
					t.Fatal("existing redirect/PoW path did not reach the document")
				}
				if err == nil || !strings.Contains(err.Error(), "client-config") || errors.Is(err, ErrCaptchaRequired) || errors.Is(err, ErrLoginRequired) {
					t.Fatal("document parse failure was misclassified as an auth challenge")
				}
			})
		}
	}
}
