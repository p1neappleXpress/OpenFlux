package transport

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

type fakeExchanger struct {
	mu  sync.Mutex
	jar map[string]string
}

func (f *fakeExchanger) FetchCookies() (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]string, len(f.jar))
	for k, v := range f.jar {
		out[k] = v
	}
	return out, nil
}

func (f *fakeExchanger) ApplyCookies(jar map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jar = make(map[string]string, len(jar))
	for k, v := range jar {
		f.jar[k] = v
	}
	return nil
}

func TestCookieStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies-test.json")

	s, err := NewCookieStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Load("doc1"); got != nil {
		t.Fatalf("missing key returned %v", got)
	}
	if err := s.Save("doc1", map[string]string{"a": "1", "b": "2"}); err != nil {
		t.Fatal(err)
	}

	// Reload from disk.
	s2, err := NewCookieStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got := s2.Load("doc1")
	if got["a"] != "1" || got["b"] != "2" {
		t.Fatalf("reload mismatch: %v", got)
	}

	// Delete.
	if err := s2.Delete("doc1"); err != nil {
		t.Fatal(err)
	}
	s3, _ := NewCookieStore(path)
	if s3.Load("doc1") != nil {
		t.Fatal("delete did not persist")
	}
}

func TestCookieStoreCorruptFileStartsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(path, []byte("{ this is not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewCookieStore(path)
	if err != nil {
		t.Fatalf("corrupt file must not fail startup: %v", err)
	}
	if s.Load("anything") != nil {
		t.Fatal("expected empty store")
	}
}

func TestCookieStoreMissingFileIsFine(t *testing.T) {
	dir := t.TempDir()
	s, err := NewCookieStore(filepath.Join(dir, "does-not-exist.json"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Load("k") != nil {
		t.Fatal("expected empty store")
	}
}

func TestPersistentCookieExchangerPrefersLiveThenFallsBack(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies-test.json")
	store, _ := NewCookieStore(path)

	live := &fakeExchanger{}
	ex := NewPersistentCookieExchanger(live, store, "doc1")

	// Live is empty, store is empty -> nil.
	if jar, err := ex.FetchCookies(); err != nil || jar != nil {
		t.Fatalf("expected nil, nil; got %v, %v", jar, err)
	}

	// Populate store directly.
	if err := store.Save("doc1", map[string]string{"cached": "yes"}); err != nil {
		t.Fatal(err)
	}
	jar, err := ex.FetchCookies()
	if err != nil || jar["cached"] != "yes" {
		t.Fatalf("fallback to store failed: %v %v", jar, err)
	}

	// Populate live; live wins.
	if err := live.ApplyCookies(map[string]string{"live": "yes"}); err != nil {
		t.Fatal(err)
	}
	jar, _ = ex.FetchCookies()
	if jar["live"] != "yes" || jar["cached"] != "" {
		t.Fatalf("live should take precedence: %v", jar)
	}
}

func TestPersistentCookieExchangerSavesOnApply(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies-test.json")
	store, _ := NewCookieStore(path)
	live := &fakeExchanger{}
	ex := NewPersistentCookieExchanger(live, store, "doc1")

	if err := ex.ApplyCookies(map[string]string{"x": "1"}); err != nil {
		t.Fatal(err)
	}

	// Persisted?
	reloaded, _ := NewCookieStore(path)
	if reloaded.Load("doc1")["x"] != "1" {
		t.Fatal("ApplyCookies did not persist")
	}
	// Applied live?
	liveJar, _ := live.FetchCookies()
	if liveJar["x"] != "1" {
		t.Fatal("ApplyCookies did not reach the live transport")
	}
}

func TestCookieStoreConcurrentSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies-test.json")
	store, _ := NewCookieStore(path)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = store.Save("k", map[string]string{"n": string(rune('a' + i))})
		}(i)
	}
	wg.Wait()

	// File must be valid JSON at the end.
	if _, err := NewCookieStore(path); err != nil {
		t.Fatal(err)
	}
}
