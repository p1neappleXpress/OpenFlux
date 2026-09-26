package yandex

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"openflux/transport"
)

func writerLoops() int {
	buf := make([]byte, 1<<20)
	return strings.Count(string(buf[:runtime.Stack(buf, true)]), "(*YandexDocsTransport).writerLoop")
}

// Stop must close the document connection and end the writer, not leave
// the socket (a participant in the document) and goroutines behind.
func TestStopClosesDocumentConnection(t *testing.T) {
	closed := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				close(closed)
				return
			}
		}
	}))
	defer srv.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	tr := NewYandexDocsTransport("https://docs.example/d", transport.DefaultConfig())
	if err := tr.BaseTransport.Start(); err != nil {
		t.Fatal(err)
	}
	tr.session = &DocSession{Conn: conn, WriteQueue: make(chan []byte, 1)}
	before := writerLoops()
	go tr.writerLoop()
	deadline := time.Now().Add(2 * time.Second)
	for writerLoops() == before && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	if err := tr.Stop(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("document connection still open after Stop")
	}
	deadline = time.Now().Add(2 * time.Second)
	for writerLoops() > before && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if writerLoops() > before {
		t.Fatal("writer goroutine still running after Stop")
	}
}
