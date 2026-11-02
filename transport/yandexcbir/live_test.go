package yandexcbir

import (
	"bytes"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"testing"
	"time"
)

// TestLiveCarrier checks the storage path end to end without slots: a payload
// goes up as a PNG and comes back byte for byte. Skipped unless
// OPENFLUX_CBIR_LIVE=1 (it talks to Yandex for real).
func TestLiveCarrier(t *testing.T) {
	if os.Getenv("OPENFLUX_CBIR_LIVE") != "1" {
		t.Skip("set OPENFLUX_CBIR_LIVE=1 to run against Yandex")
	}
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Timeout: 60 * time.Second, Jar: jar}
	// Warm the cookie jar the way a browser would: the upload endpoint likes a
	// session from the images page.
	if req, err := http.NewRequest("GET", "https://yandex.ru/images/", nil); err == nil {
		req.Header.Set("User-Agent", userAgent)
		if res, err := c.Do(req); err == nil {
			_, _ = io.Copy(io.Discard, res.Body)
			res.Body.Close()
		}
	}
	payload := append([]byte{0}, bytes.Repeat([]byte("OpenFlux cbir carrier\n"), 400)...)
	id, err := upload(c, payload)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	t.Logf("cbirId=%s", id)
	got, err := download(c, id)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload changed: got %d bytes, want %d", len(got), len(payload))
	}
	t.Logf("round trip ok: %d bytes", len(got))
}
