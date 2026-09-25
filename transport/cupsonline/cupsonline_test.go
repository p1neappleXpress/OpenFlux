package cupsonline

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"universal-bypass-tool/transport"
)

const (
	roomA = "11111111-1111-1111-1111-111111111111"
	roomB = "22222222-2222-2222-2222-222222222222"
)

func TestParseRoomListAcceptsEveryForm(t *testing.T) {
	packed := packRooms([]string{roomA, roomB})
	for name, in := range map[string]string{
		"bare base64":  packed,
		"?rooms=":      "https://example/?rooms=" + packed,
		"surrounding ": "  " + packed + "\n",
	} {
		got := parseRoomList(in)
		if strings.Join(got, ",") != roomA+","+roomB {
			t.Errorf("%s: got %q", name, got)
		}
	}
	if got := parseRoomList("https://example/?room=" + roomA); len(got) != 1 || got[0] != roomA {
		t.Errorf("?room=: got %q", got)
	}
	for _, in := range []string{"", "http://#", "not base64 at all"} {
		if got := parseRoomList(in); len(got) != 0 {
			t.Errorf("%q: want no rooms, got %q", in, got)
		}
	}
}

// An exit node given the printed list must re-join those rooms rather than
// create new ones; without one it creates. A client can't start without one.
func TestExitNodeReusesRoomsFromURL(t *testing.T) {
	packed := packRooms([]string{roomA, roomB})
	cfg := transport.DefaultConfig()

	exit := NewCupsonlineTransport(packed, cfg, false)
	if len(exit.roomIDs) != 2 || exit.clientErr != nil {
		t.Fatalf("exit with a list: roomIDs=%q err=%v", exit.roomIDs, exit.clientErr)
	}
	fresh := NewCupsonlineTransport("http://#", cfg, false)
	if len(fresh.roomIDs) != 0 || fresh.clientErr != nil {
		t.Fatalf("exit without a list must create rooms: roomIDs=%q err=%v", fresh.roomIDs, fresh.clientErr)
	}
	if client := NewCupsonlineTransport("", cfg, true); client.clientErr == nil {
		t.Fatal("a client without a room list must refuse to start")
	}
}

func TestAuthorizeReportsGoneRooms(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"404":        func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) },
		"no uuid":    func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html>room closed</html>")) },
		"server 502": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) },
	} {
		srv := httptest.NewServer(handler)
		_, err := authorize(context.Background(), srv.URL, nil)
		srv.Close()
		gone := errors.Is(err, errRoomGone)
		if wantGone := name != "server 502"; gone != wantGone {
			t.Errorf("%s: gone=%v want %v (err %v)", name, gone, wantGone, err)
		}
	}
}

func TestReadReplySurfacesRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		conn.WriteMessage(websocket.TextMessage, []byte(`{"id":1,"connect":{"client":"x"}}`))
		conn.WriteMessage(websocket.TextMessage, []byte(`{"id":2,"error":{"code":109,"message":"token expired"}}`))
		conn.ReadMessage()
	}))
	defer srv.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if err := readReply(conn, time.Second, 1, "connect", nil); err != nil {
		t.Fatalf("a normal reply must pass: %v", err)
	}
	err = readReply(conn, time.Second, 2, "subscribe", nil)
	if err == nil || !strings.Contains(err.Error(), "token expired") || !errors.Is(err, errRefused) {
		t.Fatalf("a refusal must come back as errRefused, got %v", err)
	}
}

func TestFmtUptime(t *testing.T) {
	for d, want := range map[time.Duration]string{
		47*time.Hour + 12*time.Minute: "47ч12м",
		3*time.Minute + 5*time.Second: "3м05с",
	} {
		if got := fmtUptime(d); got != want {
			t.Errorf("%v: got %q want %q", d, got, want)
		}
	}
}
