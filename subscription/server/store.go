package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// State is everything the service remembers, one JSON file.
type State struct {
	Users  map[string]*User  `json:"users"`  // by id
	Keys   map[string]*Key   `json:"keys"`   // by id
	Orders map[string]*Order `json:"orders"` // by id
}

// User is an account. The password and the recovery key are stored hashed.
type User struct {
	ID           string   `json:"id"`
	Login        string   `json:"login"` // lowercase, unique
	PassHash     string   `json:"passHash"`
	RecoveryHash string   `json:"recoveryHash"`
	CreatedAt    int64    `json:"createdAt"`
	PlanID       string   `json:"planId,omitempty"`
	Expires      int64    `json:"expires"`
	TrialUsed    bool     `json:"trialUsed,omitempty"`
	Banned       bool     `json:"banned,omitempty"`
	Note         string   `json:"note,omitempty"`
	Devices      []Device `json:"devices"`
}

// Device is where the account is signed in. Only the token's SHA-256 is kept.
type Device struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	TokenHash string `json:"tokenHash"`
	CreatedAt int64  `json:"createdAt"`
	LastSeen  int64  `json:"lastSeen"`
}

// Key is a subscription without an account (a gift, a handout). Only the
// token's SHA-256 is kept.
type Key struct {
	ID        string   `json:"id"`
	TokenHash string   `json:"tokenHash"`
	Name      string   `json:"name"`
	Expires   int64    `json:"expires"`
	Revoked   bool     `json:"revoked,omitempty"`
	Pools     []string `json:"pools,omitempty"`
	CreatedAt int64    `json:"createdAt"`
}

// Order is a purchase waiting for payment, or paid.
type Order struct {
	ID        string `json:"id"`
	UserID    string `json:"userId"`
	PlanID    string `json:"planId"`
	Status    string `json:"status"` // pending, paid, canceled
	CreatedAt int64  `json:"createdAt"`
	PaidAt    int64  `json:"paidAt,omitempty"`
}

type store struct {
	mu    sync.Mutex
	path  string
	state State
}

func openStore(dir string) (*store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &store{path: filepath.Join(dir, "state.json")}
	b, err := os.ReadFile(s.path)
	switch {
	case err == nil:
		if err := json.Unmarshal(b, &s.state); err != nil {
			return nil, err
		}
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}
	if s.state.Users == nil {
		s.state.Users = map[string]*User{}
	}
	if s.state.Keys == nil {
		s.state.Keys = map[string]*Key{}
	}
	if s.state.Orders == nil {
		s.state.Orders = map[string]*Order{}
	}
	return s, nil
}

// save writes the state atomically; the caller holds mu.
func (s *store) save() error {
	b, err := json.MarshalIndent(&s.state, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *store) userByLogin(login string) *User {
	login = normLogin(login)
	for _, u := range s.state.Users {
		if u.Login == login {
			return u
		}
	}
	return nil
}

// userByToken finds the account and device a device token belongs to.
func (s *store) userByToken(token string) (*User, *Device) {
	h := hashToken(token)
	for _, u := range s.state.Users {
		for i := range u.Devices {
			if u.Devices[i].TokenHash == h {
				return u, &u.Devices[i]
			}
		}
	}
	return nil, nil
}

func (s *store) keyByToken(token string) *Key {
	h := hashToken(token)
	for _, k := range s.state.Keys {
		if k.TokenHash == h {
			return k
		}
	}
	return nil
}

func (s *store) sortedUsers() []*User {
	out := make([]*User, 0, len(s.state.Users))
	for _, u := range s.state.Users {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out
}

func normLogin(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

// randToken is n random bytes, URL-safe.
func randToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func newID() string { return randToken(9) }
