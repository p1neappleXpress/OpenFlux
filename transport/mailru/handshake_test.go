package mailru

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestWaitMailruSocketIOHandshakeOrder(t *testing.T) {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}

	serverErr := make(chan error, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()

		// Mail.ru sends the Engine.IO open frame first.
		if err := conn.WriteMessage(
			websocket.TextMessage,
			[]byte(`0{"sid":"engine-session","upgrades":[]}`),
		); err != nil {
			serverErr <- err
			return
		}

		// The client must not send Socket.IO connect until the open frame
		// above has been consumed.
		_, message, err := conn.ReadMessage()
		if err != nil {
			serverErr <- err
			return
		}

		want := `40{"token":"test-token"}`
		if got := string(message); got != want {
			serverErr <- fmt.Errorf("Socket.IO connect = %q, want %q", got, want)
			return
		}

		if err := conn.WriteMessage(
			websocket.TextMessage,
			[]byte(`40{"sid":"socket-session"}`),
		); err != nil {
			serverErr <- err
			return
		}

		serverErr <- nil
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	session := &DocSession{
		Conn: conn,
	}

	if err := waitMailruSocketIO(session, "test-token"); err != nil {
		t.Fatalf("waitMailruSocketIO: %v", err)
	}

	select {
	case err := <-serverErr:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for WebSocket test server")
	}
}
