package yandex

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
)

// The PoW captcha must be answered on the host that issued it (regional
// domains such as docs.yandex.kz reject a POST to docs.yandex.ru), and
// relative redirects must be followed on that host too.
func TestSolveCaptchaPostsToIssuingHost(t *testing.T) {
	ssr := base64.StdEncoding.EncodeToString([]byte(
		`{"uniqueKey":"k","action":"a","pow":{"complexity":0,"prefix":"00"},"timestamp":1}`))
	var srv *httptest.Server
	posted := make(chan string, 1)
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/edit/d/doc":
			http.Redirect(w, r, "/showcaptchafast?d=1", http.StatusFound)
		case r.URL.Path == "/showcaptchafast":
			fmt.Fprintf(w, `<script>window.__SSR_DATA__ = JSON.parse(atob("%s"))</script>`+
				`<form id="tmgrdfrend-form" action="/checkcaptchafast?a=1&amp;b=2"></form>`, ssr)
		case r.URL.Path == "/checkcaptchafast" && r.Method == http.MethodPost:
			posted <- r.Header.Get("Origin")
			http.Redirect(w, r, "/edit/d/doc?ok=1", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	jar, _ := cookiejar.New(nil)
	retpath, err := solveCaptcha(srv.URL+"/edit/d/doc", jar, "")
	if err != nil {
		t.Fatalf("solveCaptcha: %v", err)
	}
	select {
	case origin := <-posted:
		if origin != srv.URL {
			t.Fatalf("Origin = %q, want %q", origin, srv.URL)
		}
	default:
		t.Fatal("the answer was not posted to the issuing host")
	}
	if !strings.HasSuffix(retpath, "/edit/d/doc?ok=1") {
		t.Fatalf("retpath = %q", retpath)
	}
}
