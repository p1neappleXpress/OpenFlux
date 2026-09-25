package yandex

import (
	"errors"
	"net/http"
	"net/http/httptest"
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
