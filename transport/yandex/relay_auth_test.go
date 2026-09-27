package yandex

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// An expired token turns every relay POST into 401 (#64, #109). The relay
// must ask for a fresh authorization once, not once per worker, and send
// the rejected batch again with it.
func TestRelayRefreshesAuthorizationOn401(t *testing.T) {
	var shared atomic.Pointer[volgaAuth]
	shared.Store(&volgaAuth{Token: "expired", RequestPath: "p"})

	var refreshes atomic.Int32
	var mu sync.Mutex
	var sent []string
	relay := &relayClient{
		auth:     &shared,
		ctx:      context.Background(),
		stats:    &VolgaStats{},
		authWait: 2 * time.Second,
		httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
			mu.Lock()
			sent = append(sent, token)
			mu.Unlock()
			status := 204
			if token == "expired" {
				status = 401
			}
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
		})},
	}
	relay.onAuthRejected = func() {
		refreshes.Add(1)
		go func() {
			time.Sleep(100 * time.Millisecond)
			shared.Store(&volgaAuth{Token: "fresh", RequestPath: "p"})
		}()
	}

	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- relay.deliver([][]byte{{1, 2, 3}})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("batch lost after a refresh: %v", err)
		}
	}
	if n := refreshes.Load(); n != 1 {
		t.Fatalf("asked for a new authorization %d times, want 1", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if last := sent[len(sent)-1]; last != "fresh" {
		t.Fatalf("retry used token %q", last)
	}
}

// A rejection with no refresh in sight gives up after authWait instead of
// holding the worker forever.
func TestRelayGivesUpWhenNoNewAuthorization(t *testing.T) {
	var shared atomic.Pointer[volgaAuth]
	shared.Store(&volgaAuth{Token: "expired", RequestPath: "p"})
	relay := &relayClient{
		auth:     &shared,
		ctx:      context.Background(),
		stats:    &VolgaStats{},
		authWait: 200 * time.Millisecond,
		httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
		})},
	}
	start := time.Now()
	if err := relay.deliver([][]byte{{1}}); err == nil {
		t.Fatal("deliver succeeded with a rejected token")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("deliver waited far past authWait")
	}
}
