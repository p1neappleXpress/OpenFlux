package yandex

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"openflux/transport"
)

// connectVia runs connectToDoc against srv and returns the first socket.io
// frame the server received, or "" if none arrived in time.
func connectVia(t *testing.T, srv *httptest.Server, got <-chan string) string {
	t.Helper()
	host := strings.TrimPrefix(srv.URL, "http://")
	orig := fetchDocInfo
	fetchDocInfo = func(*YandexDocsTransport, string, string) (YandexDocsInfo, error) {
		return YandexDocsInfo{
			Host:   host,
			Origin: srv.URL,
			Token:  "tok",
			WsURL:  "ws://" + host + "/doc/c/?EIO=4&transport=websocket",
		}, nil
	}
	defer func() { fetchDocInfo = orig }()

	tr := NewYandexDocsTransport("https://docs.example/d", transport.DefaultConfig())
	if err := tr.BaseTransport.Start(); err != nil {
		t.Fatal(err)
	}
	defer tr.Stop()
	tr.connectToDoc(0)
	select {
	case f := <-got:
		return f
	case <-time.After(5 * time.Second):
		return ""
	}
}

// A strict balancer only lets a websocket in that carries the sid from a
// polling handshake and upgrades it with 2probe/3probe/5.
func TestConnectDoesEngineIOPollingHandshake(t *testing.T) {
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("transport") == "polling" {
			http.SetCookie(w, &http.Cookie{Name: "io", Value: "S1"})
			fmt.Fprint(w, `0{"sid":"S1","upgrades":["websocket"],"pingInterval":25000}`)
			return
		}
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		if q.Get("sid") != "S1" || !strings.Contains(r.Header.Get("Cookie"), "io=S1") {
			return // strict: no polling session, close right away
		}
		want := []string{"2probe", "5"}
		for i, w := range want {
			_, m, err := c.ReadMessage()
			if err != nil || string(m) != w {
				return
			}
			if i == 0 {
				c.WriteMessage(websocket.TextMessage, []byte("3probe"))
			}
		}
		_, m, err := c.ReadMessage()
		if err == nil {
			got <- string(m)
		}
		c.ReadMessage()
	}))
	defer srv.Close()

	if f := connectVia(t, srv, got); !strings.HasPrefix(f, `40{"token":"tok"}`) {
		t.Fatalf("socket.io CONNECT never reached a strict balancer (got %q)", f)
	}
}

// Without polling the client must wait for the server's OPEN packet before
// it sends socket.io frames.
func TestConnectWaitsForEngineIOOpen(t *testing.T) {
	got := make(chan string, 1)
	var early atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("transport") == "polling" {
			http.NotFound(w, r)
			return
		}
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		frames := make(chan string, 4)
		go func() {
			for {
				_, m, err := c.ReadMessage()
				if err != nil {
					close(frames)
					return
				}
				frames <- string(m)
			}
		}()
		select {
		case <-frames:
			early.Store(true)
			got <- "early"
			return
		case <-time.After(300 * time.Millisecond):
		}
		c.WriteMessage(websocket.TextMessage, []byte(`0{"sid":"S2"}`))
		if m, ok := <-frames; ok {
			got <- m
		}
		for range frames {
		}
	}))
	defer srv.Close()

	f := connectVia(t, srv, got)
	if f == "early" {
		t.Fatal("client wrote before the engine.io OPEN packet")
	}
	if !strings.HasPrefix(f, `40{"token":"tok"}`) {
		t.Fatalf("socket.io CONNECT not sent after OPEN (got %q)", f)
	}
}
