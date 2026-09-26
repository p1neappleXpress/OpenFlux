package transport

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"openflux/utils"
)

// CookieStore is a small persistent store for per-session cookie jars.
//
// It is used by transports that speak HTTP in some form (Yandex.Docs,
// Volga, Mail.ru, Cups, Boards). The file is a single JSON object mapping a
// session key (usually the document URL) to a name -> value cookie map.
//
// Writes are atomic: a temp file is written and renamed over the target, so
// a crash mid-write cannot leave a truncated JSON behind.
type CookieStore struct {
	path string

	mu  sync.RWMutex
	jar map[string]map[string]string
}

// NewCookieStore loads (or creates in memory) the store at path. A missing
// file is not an error; a corrupt file is reported and the store starts
// empty, so a bad file never blocks startup.
func NewCookieStore(path string) (*CookieStore, error) {
	if path == "" {
		return nil, errors.New("cookiestore: empty path")
	}
	s := &CookieStore{
		path: path,
		jar:  make(map[string]map[string]string),
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("cookiestore: read %s: %w", path, err)
	}
	if len(data) == 0 {
		return s, nil
	}
	var loaded map[string]map[string]string
	if err := json.Unmarshal(data, &loaded); err != nil {
		utils.Debugf("[COOKIE] %s is not valid JSON (%v); starting empty", path, err)
		return s, nil
	}
	s.jar = loaded
	return s, nil
}

// Path returns the on-disk path of the store.
func (s *CookieStore) Path() string { return s.path }

// Load returns a copy of the jar for key, or nil if the key is unknown.
func (s *CookieStore) Load(key string) map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	src := s.jar[key]
	if src == nil {
		return nil
	}
	out := make(map[string]string, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

// Save replaces the jar for key and persists the whole store to disk.
func (s *CookieStore) Save(key string, jar map[string]string) error {
	if key == "" {
		return errors.New("cookiestore: empty key")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(jar) == 0 {
		delete(s.jar, key)
	} else {
		cp := make(map[string]string, len(jar))
		for k, v := range jar {
			cp[k] = v
		}
		s.jar[key] = cp
	}
	return s.persistLocked()
}

// Delete removes a key from the store and persists.
func (s *CookieStore) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.jar[key]; !ok {
		return nil
	}
	delete(s.jar, key)
	return s.persistLocked()
}

// All returns a deep copy of the whole store (useful for debugging).
func (s *CookieStore) All() map[string]map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]map[string]string, len(s.jar))
	for k, v := range s.jar {
		cp := make(map[string]string, len(v))
		for kk, vv := range v {
			cp[kk] = vv
		}
		out[k] = cp
	}
	return out
}

// persistLocked writes the store atomically. Caller holds s.mu.
func (s *CookieStore) persistLocked() error {
	dir := filepath.Dir(s.path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("cookiestore: mkdir %s: %w", dir, err)
		}
	}
	data, err := json.MarshalIndent(s.jar, "", "  ")
	if err != nil {
		return fmt.Errorf("cookiestore: marshal: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("cookiestore: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("cookiestore: rename %s -> %s: %w", tmp, s.path, err)
	}
	return nil
}

// PersistentCookieExchanger wraps a CookieExchanger with a CookieStore: reads
// fall back to disk when the live transport has nothing yet (e.g. after a
// restart), and every successful ApplyCookies is persisted.
type PersistentCookieExchanger struct {
	inner CookieExchanger
	store *CookieStore
	key   string
}

// NewPersistentCookieExchanger wires inner to store under key. If store or
// inner is nil, the inner is returned unchanged.
func NewPersistentCookieExchanger(inner CookieExchanger, store *CookieStore, key string) CookieExchanger {
	if store == nil || inner == nil {
		return inner
	}
	return &PersistentCookieExchanger{inner: inner, store: store, key: key}
}

// FetchCookies returns the live transport jar if non-empty, else the stored
// jar for this key.
func (p *PersistentCookieExchanger) FetchCookies() (map[string]string, error) {
	if jar, err := p.inner.FetchCookies(); err == nil && len(jar) > 0 {
		return jar, nil
	} else if err != nil && len(jar) == 0 {
		if cached := p.store.Load(p.key); cached != nil {
			return cached, nil
		}
		return nil, err
	}
	if cached := p.store.Load(p.key); cached != nil {
		return cached, nil
	}
	return nil, nil
}

// ApplyCookies applies to the live transport first, then persists. If the
// transport rejects the jar, the store is not touched.
func (p *PersistentCookieExchanger) ApplyCookies(jar map[string]string) error {
	if err := p.inner.ApplyCookies(jar); err != nil {
		return err
	}
	return p.store.Save(p.key, jar)
}

// Persistent returns the underlying PersistentCookieExchanger, if any.
func Persistent(ex CookieExchanger) *PersistentCookieExchanger {
	if p, ok := ex.(*PersistentCookieExchanger); ok {
		return p
	}
	return nil
}
