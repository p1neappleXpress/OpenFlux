package yandex

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A redirect to showcaptcha?cc=1 (SmartCaptcha, second tier) must be returned
// as ErrCaptchaRequired, not as a generic error.
func TestFetchDocInfoSmartCaptchaReturnsSentinel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/doc":
			http.Redirect(w, r, "/showcaptcha?cc=1&form-fb-hint=2.73&mt=deadbeef", http.StatusFound)
		case "/showcaptcha":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("<html><body>captcha</body></html>"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	tr := &YandexDocsTransport{url: srv.URL + "/doc"}
	_, err := tr.fetchDocInfo(srv.URL+"/doc", "0000000001")
	if !errors.Is(err, ErrCaptchaRequired) {
		t.Fatalf("expected ErrCaptchaRequired, got %v", err)
	}
}

// A redirect to passport.yandex.ru (login page) must be returned as
// ErrLoginRequired.
func TestFetchDocInfoLoginPageReturnsSentinel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/doc":
			http.Redirect(w, r, "/passport.yandex/auth", http.StatusFound)
		case "/passport.yandex/auth":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("<html>login</html>"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	tr := &YandexDocsTransport{url: srv.URL + "/doc"}
	_, err := tr.fetchDocInfo(srv.URL+"/doc", "0000000001")
	if !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("expected ErrLoginRequired, got %v", err)
	}
}

// powFailHandler redirects /doc (and the boards /whiteboard/) to captchaURL
// and answers everything else with 403, so the internal PoW solver fails.
func powFailHandler(captchaURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/doc", "/whiteboard/":
			http.Redirect(w, r, captchaURL, http.StatusFound)
		default:
			w.WriteHeader(http.StatusForbidden)
		}
	}
}

// checkPoWFailErr wants ErrCaptchaRequired with the solver's cause kept.
func checkPoWFailErr(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrCaptchaRequired) {
		t.Fatalf("expected ErrCaptchaRequired, got %v", err)
	}
	if !strings.Contains(err.Error(), "captcha showcaptcha status 403") {
		t.Fatalf("solver cause lost: %v", err)
	}
}

// A redirect to showcaptchafast whose PoW solve fails must still be returned
// as ErrCaptchaRequired, so the external solver is asked.
func TestFetchDocInfoFailedPoWReturnsSentinel(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		powFailHandler(srv.URL+"/showcaptchafast?test=1")(w, r)
	}))
	defer srv.Close()

	tr := &YandexDocsTransport{url: srv.URL + "/doc"}
	_, err := tr.fetchDocInfo(srv.URL+"/doc", "0000000001")
	checkPoWFailErr(t, err)
}

// Same for the Volga (vyandex) document authorization.
func TestAuthorizeWithJarFailedPoWReturnsSentinel(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		powFailHandler(srv.URL+"/showcaptchafast?test=1")(w, r)
	}))
	defer srv.Close()

	_, err := authorizeWithJar(srv.URL+"/doc", nil)
	checkPoWFailErr(t, err)
}

// Same for the boards authorization.
func TestBoardsAuthorizeFailedPoWReturnsSentinel(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		powFailHandler(srv.URL+"/showcaptchafast?test=1")(w, r)
	}))
	defer srv.Close()

	old := boardsBaseURL
	boardsBaseURL = srv.URL
	t.Cleanup(func() { boardsBaseURL = old })

	_, err := (&BoardsTransport{}).authorize("testhash", "guest")
	checkPoWFailErr(t, err)
}
