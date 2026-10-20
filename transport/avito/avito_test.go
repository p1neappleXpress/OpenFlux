package avito

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport"
)

// avitoRecorder records when each room creation and WebSocket handshake came.
type avitoRecorder struct {
	mu    sync.Mutex
	rooms []time.Time
	dials []time.Time
}

func (f *avitoRecorder) roomTimes() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Time(nil), f.rooms...)
}

func (f *avitoRecorder) dialTimes() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Time(nil), f.dials...)
}

// startAvitoRecorder answers the n-th room creation (from 1) with room(w, n) and
// the n-th WebSocket handshake with ws(w, n), and points baseURL at it for
// the test.
func startAvitoRecorder(t *testing.T, room, ws func(w http.ResponseWriter, n int)) *avitoRecorder {
	t.Helper()
	f := &avitoRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		switch r.URL.Path {
		case "/web/1/code/room":
			f.rooms = append(f.rooms, time.Now())
			n := len(f.rooms)
			f.mu.Unlock()
			room(w, n)
		case "/sharedb":
			f.dials = append(f.dials, time.Now())
			n := len(f.dials)
			f.mu.Unlock()
			ws(w, n)
		default:
			f.mu.Unlock()
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	old := baseURL
	baseURL = srv.URL
	t.Cleanup(func() { baseURL = old })
	return f
}

func roomOK(w http.ResponseWriter, n int) {
	_, _ = fmt.Fprintf(w, `{"room":{"id":"room-%08d"}}`, n)
}

func roomForbidden(w http.ResponseWriter, _ int) {
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte("<html>403 Forbidden</html>"))
}

// newTestExit is an exit that creates 2 rooms with short pauses.
func newTestExit() *AvitoTransport {
	tr := NewAvitoTransport("", transport.DefaultConfig(), "", false)
	tr.config.NumRooms = 1
	tr.config.RoomCreatePause = 10 * time.Millisecond
	return tr
}

// A 403 is permanent: room creation stops after the first request and says
// why.
func TestEnterRoomsStopsOn403(t *testing.T) {
	f := startAvitoRecorder(t, roomForbidden, nil)
	_, err := newTestExit().enterRooms()
	if err == nil {
		t.Fatal("expected an error")
	}
	if n := len(f.roomTimes()); n != 1 {
		t.Fatalf("%d room requests after 403, want 1", n)
	}
	if !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "User-Agent") {
		t.Fatalf("unclear error: %v", err)
	}
}

func TestCreateRoomListStopsOn403(t *testing.T) {
	f := startAvitoRecorder(t, roomForbidden, nil)
	_, err := CreateRoomList(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "User-Agent") {
		t.Fatalf("unclear error: %v", err)
	}
	if n := len(f.roomTimes()); n != 1 {
		t.Fatalf("%d room requests after 403, want 1", n)
	}
}

// A 429 with Retry-After pauses room creation for that long, then it goes on.
func TestEnterRoomsWaitsRetryAfterOn429(t *testing.T) {
	f := startAvitoRecorder(t, func(w http.ResponseWriter, n int) {
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		roomOK(w, n)
	}, nil)
	ids, err := newTestExit().enterRooms()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Fatalf("got %d rooms, want 2", len(ids))
	}
	times := f.roomTimes()
	if len(times) != 3 {
		t.Fatalf("%d room requests, want 3", len(times))
	}
	if gap := times[1].Sub(times[0]); gap < 900*time.Millisecond {
		t.Fatalf("retried %v after 429 with Retry-After: 1", gap)
	}
}

// 200 answers: rooms are created as before.
func TestEnterRoomsCreatesRooms(t *testing.T) {
	f := startAvitoRecorder(t, roomOK, nil)
	ids, err := newTestExit().enterRooms()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || len(f.roomTimes()) != 2 {
		t.Fatalf("got %d rooms in %d requests, want 2 in 2", len(ids), len(f.roomTimes()))
	}
}

// handshakesWithin starts an exit on rooms rooms whose WebSocket handshakes
// all get ws, and returns how many handshakes came within d of the first one.
// Reconnect delays are tiny, so only the shared pause can hold the rooms.
func handshakesWithin(t *testing.T, rooms int, d time.Duration, ws func(w http.ResponseWriter, n int)) int {
	t.Helper()
	f := startAvitoRecorder(t, roomOK, ws)
	tr := NewAvitoTransport("", transport.DefaultConfig(), "", false)
	for i := 0; i < rooms; i++ {
		tr.roomIDs = append(tr.roomIDs, fmt.Sprintf("room-%08d", i))
	}
	tr.config.ReconnectMinDelay = 20 * time.Millisecond
	tr.config.ReconnectMaxDelay = 50 * time.Millisecond // ThrottleMinDelay stays 2s
	if err := tr.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(d)
	_ = tr.Stop()

	times := f.dialTimes()
	if len(times) == 0 {
		t.Fatal("no handshakes")
	}
	n := 0
	for _, at := range times {
		if at.Sub(times[0]) < d {
			n++
		}
	}
	return n
}

// A 429 on the WebSocket handshake pauses every room for Retry-After instead
// of reconnecting from ReconnectMinDelay: only the first round comes before
// it ends. A slow machine can only make fewer handshakes, not more.
func TestWSHandshake429WaitsRetryAfter(t *testing.T) {
	n := handshakesWithin(t, 2, 1500*time.Millisecond, func(w http.ResponseWriter, _ int) {
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	if n > 2 {
		t.Fatalf("%d handshakes from 2 rooms within 1.5s of a 429 with Retry-After: 2", n)
	}
}

// A 403 on the WebSocket handshake pauses every room too (ThrottleMinDelay
// without Retry-After), so a lasting 403 does not have each room knock every
// ReconnectMaxDelay.
func TestWSHandshake403PausesAllRooms(t *testing.T) {
	n := handshakesWithin(t, 4, 1500*time.Millisecond, func(w http.ResponseWriter, _ int) {
		w.WriteHeader(http.StatusForbidden)
	})
	if n > 4 {
		t.Fatalf("%d handshakes from 4 rooms within 1.5s of a 403 (pause 2s)", n)
	}
}
