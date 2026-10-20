package mailru

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport"
)

const anonymousBody = `{"status":null,"error":"FORBIDDEN/ANONYMOUSE/ACCESS","body":{}}`

// editRequest is one r7/edit request the fake API saw.
type editRequest struct {
	at   time.Time
	csrf string // X-CSRF-Token
	seq  int    // order among all requests, csrf ones included
}

// fakeCloud is Mail.ru's API for fetchDocInfo: /csrf issues a token only to a
// request carrying the sign-in cookie, /edit answers with status and body.
type fakeCloud struct {
	srv *httptest.Server

	mu    sync.Mutex
	seq   int
	csrfs []int // seq of each csrf request
	edits []editRequest
}

const testCSRFToken = "csrf-token-1"

// startEditAPI points editAPIURL and csrfURL at a fakeCloud for the test.
func startEditAPI(t *testing.T, status int, body string) *fakeCloud {
	t.Helper()
	f := &fakeCloud{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.seq++
		seq := f.seq
		if r.URL.Path == "/csrf" {
			f.csrfs = append(f.csrfs, seq)
			f.mu.Unlock()
			if _, err := r.Cookie("sdcs"); err != nil {
				w.WriteHeader(http.StatusForbidden) // no sign-in
				return
			}
			_, _ = w.Write([]byte(`{"body":{"token":"` + testCSRFToken + `"}}`))
			return
		}
		f.edits = append(f.edits, editRequest{at: time.Now(), csrf: r.Header.Get("X-CSRF-Token"), seq: seq})
		f.mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(f.srv.Close)
	oldEdit, oldCSRF := editAPIURL, csrfURL
	editAPIURL, csrfURL = f.srv.URL+"/edit", f.srv.URL+"/csrf"
	t.Cleanup(func() { editAPIURL, csrfURL = oldEdit, oldCSRF })
	return f
}

func (f *fakeCloud) editRequests() []editRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]editRequest(nil), f.edits...)
}

func (f *fakeCloud) csrfRequests() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.csrfs...)
}

// signIn puts the sign-in cookie into the transport's jar for the fake API.
func (f *fakeCloud) signIn(t *testing.T, tr *MailruDocsTransport) {
	t.Helper()
	u, err := url.Parse(f.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	tr.cookieJar.SetCookies(u, []*http.Cookie{{Name: "sdcs", Value: "x", Path: "/"}})
}

// A 401 FORBIDDEN/ANONYMOUSE/ACCESS is reported as ErrAnonymousForbidden.
func TestFetchDocInfoAnonymousForbidden(t *testing.T) {
	startEditAPI(t, http.StatusUnauthorized, anonymousBody)
	_, err := NewMailruDocsTransport("AbCd/EfGh", transport.DefaultConfig()).fetchDocInfo("AbCd/EfGh")
	if !errors.Is(err, ErrAnonymousForbidden) {
		t.Fatalf("expected ErrAnonymousForbidden, got %v", err)
	}
}

const editOKBody = `{"api":"https://docs.example","token":"t","document":{"key":"k"},"editorConfig":{}}`

// Without a sign-in the document is opened anonymously, as before: no
// X-CSRF-Token on r7/edit.
func TestFetchDocInfoAnonymousSendsNoCSRFToken(t *testing.T) {
	f := startEditAPI(t, http.StatusOK, editOKBody)
	if _, err := NewMailruDocsTransport("AbCd/EfGh", transport.DefaultConfig()).fetchDocInfo("AbCd/EfGh"); err != nil {
		t.Fatal(err)
	}
	edits := f.editRequests()
	if len(edits) != 1 || edits[0].csrf != "" {
		t.Fatalf("anonymous r7/edit: %+v, want one without X-CSRF-Token", edits)
	}
}

// With the sign-in's cookies the CSRF token is fetched first and sent on
// r7/edit.
func TestFetchDocInfoSignedInSendsCSRFToken(t *testing.T) {
	f := startEditAPI(t, http.StatusOK, editOKBody)
	tr := NewMailruDocsTransport("AbCd/EfGh", transport.DefaultConfig())
	f.signIn(t, tr)
	if _, err := tr.fetchDocInfo("AbCd/EfGh"); err != nil {
		t.Fatal(err)
	}
	edits, csrfs := f.editRequests(), f.csrfRequests()
	if len(edits) != 1 || edits[0].csrf != testCSRFToken {
		t.Fatalf("signed-in r7/edit: %+v, want one with X-CSRF-Token %q", edits, testCSRFToken)
	}
	if len(csrfs) != 1 || csrfs[0] > edits[0].seq {
		t.Fatalf("csrf requests %v, want one before r7/edit (#%d)", csrfs, edits[0].seq)
	}
}

// Any other non-200 answer keeps its body in the error.
func TestFetchDocInfoErrorKeepsBody(t *testing.T) {
	startEditAPI(t, http.StatusInternalServerError, `{"error":"backend is down"}`)
	_, err := NewMailruDocsTransport("AbCd/EfGh", transport.DefaultConfig()).fetchDocInfo("AbCd/EfGh")
	if err == nil || !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "backend is down") {
		t.Fatalf("error without the body: %v", err)
	}
	if errors.Is(err, ErrAnonymousForbidden) {
		t.Fatalf("500 reported as anonymous refusal: %v", err)
	}
}

// While anonymous access is refused the retries are a minute apart; other
// errors keep the usual backoff.
func TestFetchRetryDelay(t *testing.T) {
	anon := fmt.Errorf("%w (status 401)", ErrAnonymousForbidden)
	if d := fetchRetryDelay(anon, 1); d != time.Minute {
		t.Fatalf("anonymous refusal: %v, want 1m", d)
	}
	other := errors.New("API returned status 500")
	for n := 1; n <= 10; n++ {
		if d := fetchRetryDelay(other, n); d > 23*time.Second {
			t.Fatalf("attempt %d: %v, want the usual backoff (at most 22.5s)", n, d)
		}
	}
}

// After an anonymous refusal the transport waits long, but cookies applied
// during the wait reconnect at once.
func TestApplyCookiesCutsAnonymousWait(t *testing.T) {
	requests := startEditAPI(t, http.StatusUnauthorized, anonymousBody).editRequests
	tr := NewMailruDocsTransport("AbCd/EfGh", transport.DefaultConfig())
	if err := tr.Start(); err != nil {
		t.Fatal(err)
	}
	defer tr.Stop()

	waitRequests := func(n int, within time.Duration) {
		t.Helper()
		deadline := time.Now().Add(within)
		for len(requests()) < n {
			if time.Now().After(deadline) {
				t.Fatalf("%d requests, want %d within %v", len(requests()), n, within)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitRequests(1, 2*time.Second)

	// The usual backoff would retry within 0.75s.
	time.Sleep(time.Second)
	if n := len(requests()); n != 1 {
		t.Fatalf("%d requests a second after the refusal, want 1", n)
	}

	if err := tr.ApplyCookies(map[string]string{"sdcs": "x"}); err != nil {
		t.Fatal(err)
	}
	waitRequests(2, time.Second)
}
