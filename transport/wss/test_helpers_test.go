package wss

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func newWebSocketPair(t *testing.T) (*websocket.Conn, *websocket.Conn) {
	t.Helper()

	serverConn := make(chan *websocket.Conn, 1)
	serverErr := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(writer, request, nil)
		if err != nil {
			serverErr <- err
			return
		}
		serverConn <- conn
	}))

	client, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		server.Close()
		t.Fatal(err)
	}

	var peer *websocket.Conn
	select {
	case peer = <-serverConn:
	case err := <-serverErr:
		_ = client.Close()
		server.Close()
		t.Fatal(err)
	case <-time.After(2 * time.Second):
		_ = client.Close()
		server.Close()
		t.Fatal("timed out waiting for server WebSocket")
	}

	t.Cleanup(func() {
		_ = client.Close()
		_ = peer.Close()
		server.Close()
	})
	return client, peer
}
