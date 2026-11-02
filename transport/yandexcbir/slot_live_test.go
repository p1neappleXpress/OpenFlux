package yandexcbir

import (
	"net/http"
	"os"
	"testing"
	"time"
)

// TestLiveSlot checks the suggest channel: publish an id into a slot and read
// it back. Skipped unless OPENFLUX_CBIR_LIVE=1 and OPENFLUX_CBIR_POOL is set.
func TestLiveSlot(t *testing.T) {
	if os.Getenv("OPENFLUX_CBIR_LIVE") != "1" {
		t.Skip("set OPENFLUX_CBIR_LIVE=1")
	}
	pool, err := ParsePool(os.Getenv("OPENFLUX_CBIR_POOL"))
	if err != nil || len(pool.C2S) == 0 {
		t.Skipf("no pool: %v", err)
	}
	c := &http.Client{Timeout: 30 * time.Second}
	s := pool.C2S[len(pool.C2S)-1]
	id := "10229179/_BhAc_byDZKo2F2uN8VLmQ941"
	if err := Publish(c, s, id); err != nil {
		t.Fatalf("publish: %v", err)
	}
	t.Logf("published %q into slot %s", id, s.ID[:8])
	time.Sleep(time.Second)
	got := Read(c, s)
	t.Logf("read back %d ids: %v", len(got), got)
	if len(got) == 0 {
		t.Fatalf("slot returned nothing")
	}
}
