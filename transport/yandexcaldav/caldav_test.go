package yandexcaldav

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport"
)

// fakeDAV is the smallest CalDAV server the transport needs: discovery, a
// calendar, and an in-memory event store with ETags.
type fakeDAV struct {
	mu     sync.Mutex
	events map[string]string // path -> body
	etags  map[string]int
	cals   map[string]string // path -> display name
}

func newFakeDAV() *fakeDAV {
	return &fakeDAV{
		events: map[string]string{},
		etags:  map[string]int{},
		cals:   map[string]string{"/calendars/test@example/events-1/": "OpenFlux"},
	}
}

func response(href, props string) string {
	return "<D:response><D:href>" + href + "</D:href><D:propstat><D:prop>" + props +
		"</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>"
}

func (f *fakeDAV) multistatus(w http.ResponseWriter, responses string) {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?><D:multistatus xmlns:D="DAV:">%s</D:multistatus>`, responses)
}

func (f *fakeDAV) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if user, pass, ok := r.BasicAuth(); !ok || user != "test@example" || pass != "secret" {
		w.Header().Set("WWW-Authenticate", `Basic realm="CalDAV"`)
		http.Error(w, "auth", http.StatusUnauthorized)
		return
	}
	path := r.URL.Path
	switch r.Method {
	case "PROPFIND":
		switch {
		case path == "/":
			f.multistatus(w, response("/", `<D:current-user-principal><D:href>/principals/users/test@example/</D:href></D:current-user-principal>`))
		case path == "/principals/users/test@example/":
			f.multistatus(w, response(path, `<C:calendar-home-set xmlns:C="urn:ietf:params:xml:ns:caldav"><D:href>/calendars/test@example/</D:href></C:calendar-home-set>`))
		case path == "/calendars/test@example/":
			f.mu.Lock()
			var b strings.Builder
			b.WriteString(response(path, `<D:displayname>Home</D:displayname><D:resourcetype><D:collection/></D:resourcetype>`))
			for cal, name := range f.cals {
				b.WriteString(response(cal, `<D:displayname>`+name+`</D:displayname><D:resourcetype><D:collection/><C:calendar xmlns:C="urn:ietf:params:xml:ns:caldav"/></D:resourcetype>`))
			}
			f.mu.Unlock()
			f.multistatus(w, b.String())
		default:
			f.mu.Lock()
			_, ok := f.events[path]
			etag := f.etags[path]
			f.mu.Unlock()
			if !ok {
				http.NotFound(w, r)
				return
			}
			f.multistatus(w, response(path, fmt.Sprintf(`<D:getetag>"%d"</D:getetag>`, etag)))
		}
	case "MKCALENDAR":
		f.mu.Lock()
		f.cals[path] = "OpenFlux"
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	case "PUT":
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.events[path] = string(body)
		f.etags[path]++
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	case "GET":
		f.mu.Lock()
		body, ok := f.events[path]
		etag := f.etags[path]
		f.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
		w.Header().Set("ETag", fmt.Sprintf(`"%d"`, etag))
		fmt.Fprint(w, body)
	case "DELETE":
		f.mu.Lock()
		delete(f.events, path)
		delete(f.etags, path)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func waitConnected(t *testing.T, tr transport.Transport, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if tr.IsConnected() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("transport did not connect in %v", timeout)
}

func TestPairExchange(t *testing.T) {
	f := newFakeDAV()
	srv := httptest.NewServer(f)
	defer srv.Close()

	params := map[string]interface{}{
		"email": "test@example", "password": "secret", "calendar": "OpenFlux",
		"server": srv.URL, "poll_ms": "50", "batch_ms": "50",
	}
	exit := New("chan", params, true, transport.DefaultConfig())
	client := New("chan", params, false, transport.DefaultConfig())
	defer exit.Stop()
	defer client.Stop()

	recv := make(chan string, 4)
	client.Receive(func(b []byte) { recv <- string(b) })
	exit.Receive(func(b []byte) { recv <- string(b) })

	if err := exit.Start(); err != nil {
		t.Fatal(err)
	}
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	waitConnected(t, exit, 10*time.Second)
	waitConnected(t, client, 10*time.Second)

	if err := exit.Send([]byte("from-exit")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-recv:
		if got != "from-exit" {
			t.Fatalf("got %q", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no packet from exit to client")
	}

	if err := client.Send([]byte("from-client")); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		select {
		case got := <-recv:
			if got == "from-client" {
				return
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatal("no packet from client to exit")
}

func TestAccountPrefix(t *testing.T) {
	params := map[string]interface{}{
		"email": "first@example", "password": "one",
		"accounts": "first@example:one|second@example:two",
	}
	tr := New("a2-lane", params, false, transport.DefaultConfig())
	if tr.email != "second@example" || tr.password != "two" {
		t.Fatalf("a2 picked %q/%q", tr.email, tr.password)
	}
	if tr.startErr != nil {
		t.Fatal(tr.startErr)
	}
	tr2 := New("a3-lane", params, false, transport.DefaultConfig())
	if tr2.startErr == nil {
		t.Fatal("a3 should be rejected with two accounts")
	}
}

func TestFoldUnfold(t *testing.T) {
	line := strings.Repeat("A", 200)
	folded := foldLine(line)
	if !strings.Contains(folded, "\r\n ") {
		t.Fatal("not folded")
	}
	if unfold(folded) != line {
		t.Fatal("unfold differs")
	}
}
