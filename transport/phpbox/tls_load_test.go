package phpbox

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// flakyCarrier refuses every nth Send with "write queue full", the way the
// Mail.ru transport does when its queue is full under load.
type flakyCarrier struct {
	pipeCarrier
	n     atomic.Int64
	every int64
	fails atomic.Int64
}

func (f *flakyCarrier) Send(p []byte) error {
	if f.n.Add(1)%f.every == 0 {
		f.fails.Add(1)
		return errFlaky
	}
	return f.pipeCarrier.Send(p)
}

var errFlaky = &net.OpError{Op: "send", Err: errString("write queue full")}

type errString string

func (e errString) Error() string { return string(e) }

// Many TLS handshakes and bodies at once through the mux, over a carrier that
// keeps pushing back: every one must complete with the right bytes. A frame
// dropped on a full queue used to break the handshake (a missing byte inside a
// TLS record), which is what "speedtest breaks under load" looked like.
func TestTLSHandshakesUnderLoadOverABusyCarrier(t *testing.T) {
	body := strings.Repeat("openflux-stream-mode-", 12000) // ~250 KB: many DATA frames each way
	want := sha256.Sum256([]byte(body))
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, body)
	}))
	defer srv.Close()
	host, portStr, _ := net.SplitHostPort(srv.Listener.Addr().String())
	port := 0
	for _, ch := range portStr {
		port = port*10 + int(ch-'0')
	}

	fc := &flakyCarrier{pipeCarrier: *newPipeCarrier(), every: 3}
	m := NewMux(fc)
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	client := &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			DisableKeepAlives: true, // a new TLS handshake every time
			TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return m.Dial(ctx, host, port)
			},
		},
	}

	const workers, each = 16, 4
	var wg sync.WaitGroup
	var bad atomic.Int64
	errs := make(chan string, workers*each)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				resp, err := client.Get("https://example.test/")
				if err != nil {
					bad.Add(1)
					errs <- err.Error()
					continue
				}
				b, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil || sha256.Sum256(b) != want {
					bad.Add(1)
					errs <- "body mismatch or read error"
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	if bad.Load() != 0 {
		var first []string
		for e := range errs {
			if len(first) < 3 {
				first = append(first, e)
			}
		}
		t.Fatalf("%d of %d requests failed under a busy carrier (carrier refused %d sends); first errors: %v",
			bad.Load(), workers*each, fc.fails.Load(), first)
	}
	if fc.fails.Load() < 50 {
		t.Fatalf("carrier refused only %d sends; the test did not exercise the busy path", fc.fails.Load())
	}
}
