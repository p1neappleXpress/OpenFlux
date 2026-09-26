package yandex

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestRelayUsesRefreshedSessionCookies(t *testing.T) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse("https://volga.yandex.ru/")
	jar.SetCookies(u, []*http.Cookie{{Name: "session", Value: "fresh"}})
	var shared atomic.Pointer[volgaAuth]
	shared.Store(&volgaAuth{Token: "fresh-token", RequestPath: "path", Session: &http.Client{Jar: jar}})
	var cookie string
	relay := &relayClient{
		auth:  &shared,
		ctx:   context.Background(),
		stats: &VolgaStats{},
		httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			cookie = req.Header.Get("Cookie")
			return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
		})},
	}
	if err := relay.sendBatch([][]byte{{1, 2, 3}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cookie, "session=fresh") {
		t.Fatalf("relay did not send refreshed session cookie: %q", cookie)
	}
}

func TestRefreshAuthPublishesNewSessionToRelay(t *testing.T) {
	var shared atomic.Pointer[volgaAuth]
	shared.Store(&volgaAuth{Token: "old"})
	relay := &relayClient{auth: &shared}
	listener := &wsListener{
		auth: &shared,
		authorizeFn: func() (*volgaAuth, error) {
			return &volgaAuth{Token: "new"}, nil
		},
	}
	if err := listener.refreshAuth(); err != nil {
		t.Fatal(err)
	}
	if got := relay.auth.Load().Token; got != "new" {
		t.Fatalf("relay token = %q, want new", got)
	}
}

func TestRefreshAuthKeepsOldSessionOnFailure(t *testing.T) {
	var shared atomic.Pointer[volgaAuth]
	shared.Store(&volgaAuth{Token: "old"})
	listener := &wsListener{
		auth: &shared,
		authorizeFn: func() (*volgaAuth, error) {
			return nil, errors.New("authorization unavailable")
		},
	}
	if err := listener.refreshAuth(); err == nil {
		t.Fatal("expected refresh error")
	}
	if got := shared.Load().Token; got != "old" {
		t.Fatalf("token = %q, want old", got)
	}
}

func TestStalledTrafficRequiresSustainedUnansweredSends(t *testing.T) {
	var detector stalledTraffic
	for i := 0; i < 12; i++ {
		if detector.Observe(0, 0) {
			t.Fatal("idle traffic triggered reconnect")
		}
	}
	for i := 0; i < 11; i++ {
		if detector.Observe(1, 0) {
			t.Fatalf("reconnect triggered early at interval %d", i)
		}
	}
	if !detector.Observe(1, 0) {
		t.Fatal("12 unanswered intervals did not trigger reconnect")
	}
	if detector.Observe(1, 1) || detector.Observe(1, 0) {
		t.Fatal("received traffic did not reset stall detector")
	}
}

func TestStalledTrafficDetectsSingleUnansweredRequest(t *testing.T) {
	var detector stalledTraffic
	if detector.Observe(1, 0) {
		t.Fatal("triggered before timeout")
	}
	for i := 0; i < 10; i++ {
		if detector.Observe(0, 0) {
			t.Fatalf("triggered early at interval %d", i)
		}
	}
	if !detector.Observe(0, 0) {
		t.Fatal("single unanswered request did not trigger reconnect")
	}
	if detector.Observe(0, 0) {
		t.Fatal("detector kept reconnecting while idle")
	}
}

func TestKeepaliveDoesNotCountAsUserTraffic(t *testing.T) {
	r := &relayClient{config: DefaultVolgaConfig(), stats: &VolgaStats{}, batchQueue: make(chan []byte, 2)}
	if err := r.Send([]byte{0}); err != nil {
		t.Fatal(err)
	}
	if got := r.stats.DataPacketsQueued.Load(); got != 0 {
		t.Fatalf("keepalive counted as data: %d", got)
	}
	if err := r.Send([]byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	if got := r.stats.DataPacketsQueued.Load(); got != 1 {
		t.Fatalf("user packet count = %d, want 1", got)
	}
}

func TestKeepaliveDoesNotCountAsReceivedUserTraffic(t *testing.T) {
	w := &wsListener{stats: &VolgaStats{}}
	item := []byte("\"" + base64.StdEncoding.EncodeToString([]byte{0, 1, 0}) + "\"")
	w.handleBundleItem(item)
	if got := w.stats.DataPacketsRecv.Load(); got != 0 {
		t.Fatalf("received keepalive counted as data: %d", got)
	}
	item = []byte("\"" + base64.StdEncoding.EncodeToString([]byte{0, 2, 1, 2}) + "\"")
	w.handleBundleItem(item)
	if got := w.stats.DataPacketsRecv.Load(); got != 1 {
		t.Fatalf("received user packets = %d, want 1", got)
	}
}

func TestReconnectDelayResetsAfterHealthySession(t *testing.T) {
	cfg := DefaultVolgaConfig()
	if got := nextReconnectDelay(cfg.ReconnectMaxDelay, 2*cfg.WSReadTimeout, cfg); got != cfg.ReconnectMinDelay {
		t.Fatalf("delay after healthy session = %v, want %v", got, cfg.ReconnectMinDelay)
	}
}

func TestRefreshDoesNotPublishAfterStop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var shared atomic.Pointer[volgaAuth]
	shared.Store(&volgaAuth{Token: "old"})
	w := &wsListener{ctx: ctx, auth: &shared, authorizeFn: func() (*volgaAuth, error) {
		return &volgaAuth{Token: "new"}, nil
	}}
	if err := w.refreshAuth(); err == nil {
		t.Fatal("expected stopped refresh to fail")
	}
	if got := shared.Load().Token; got != "old" {
		t.Fatalf("stopped refresh published %q", got)
	}
}
