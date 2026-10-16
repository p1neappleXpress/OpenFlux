package script

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dop251/goja"
	"github.com/gorilla/websocket"

	"github.com/p1neappleXpress/OpenFlux/transport"
	"github.com/p1neappleXpress/OpenFlux/transport/cupsonline"
)

// A stand-in for cups.online (room pages, subscription tokens, a Centrifugo
// that relays cursor updates to everyone in a room), enough to run the native
// transport and the JS port against each other. The same shape as the native
// package's own fake, which lives in its tests and cannot be imported.
type cupsFake struct {
	srv *httptest.Server

	mu      sync.Mutex
	users   map[string]string
	rooms   map[string]bool
	subs    map[string]map[*cupsFakeConn]bool
	created int
}

type cupsFakeConn struct {
	ws *websocket.Conn
	mu sync.Mutex
}

func (c *cupsFakeConn) send(msg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.ws.WriteMessage(websocket.TextMessage, []byte(msg))
}

func randUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func newCupsFake(t *testing.T) *cupsFake {
	t.Helper()
	f := &cupsFake{users: map[string]string{}, rooms: map[string]bool{}, subs: map[string]map[*cupsFakeConn]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/live-coding/", f.page)
	mux.HandleFunc("/sub/", f.subToken)
	mux.HandleFunc("/connection/websocket", f.centrifugo)
	f.srv = httptest.NewServer(mux)
	restore := cupsonline.SetBaseRoomURL(f.srv.URL + "/live-coding/")
	t.Cleanup(func() {
		restore()
		f.srv.CloseClientConnections()
		f.srv.Close()
	})
	return f
}

func (f *cupsFake) page(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	user := ""
	if c, err := r.Cookie("sessionid"); err == nil {
		user = f.users[c.Value]
	}
	if user == "" {
		session := randUUID()
		user = randUUID()
		f.users[session] = user
		http.SetCookie(w, &http.Cookie{Name: "sessionid", Value: session, Path: "/"})
	}
	http.SetCookie(w, &http.Cookie{Name: "csrftoken", Value: "csrf-" + user, Path: "/"})
	room := r.URL.Query().Get("room")
	if room == "" { // the base page hands out a brand-new room
		room = randUUID()
		f.rooms[room] = true
		f.created++
	} else if !f.rooms[room] {
		http.NotFound(w, r)
		return
	}
	fmt.Fprintf(w, `<html><head>
<meta name="centrifuge-connection-token" content="conn-%s">
<meta name="centrifuge-connection-url" content="%s/connection">
<meta name="centrifuge-subscription-token-url" content="%s/sub/">
</head><body><div data-room="{&quot;uuid&quot;: &quot;%s&quot;}" data-user="{&quot;uuid&quot;: &quot;%s&quot;}"></div></body></html>`,
		user, f.srv.URL, f.srv.URL, room, user)
}

func (f *cupsFake) subToken(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie("csrftoken")
	if r.Method != http.MethodPost || err != nil || r.Header.Get("X-CSRFToken") != c.Value {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var body struct {
		Channel string `json:"channel"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	_ = json.NewEncoder(w).Encode(map[string]string{"token": "sub-" + body.Channel})
}

func (f *cupsFake) centrifugo(w http.ResponseWriter, r *http.Request) {
	ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := &cupsFakeConn{ws: ws}
	defer func() {
		f.mu.Lock()
		for _, s := range f.subs {
			delete(s, c)
		}
		f.mu.Unlock()
		ws.Close()
	}()
	for {
		_, raw, err := ws.ReadMessage()
		if err != nil {
			return
		}
		if string(raw) == "{}" {
			continue
		}
		var cmd struct {
			ID        int `json:"id"`
			Connect   any `json:"connect"`
			Subscribe *struct {
				Channel string `json:"channel"`
			} `json:"subscribe"`
			RPC *struct {
				Method string `json:"method"`
				Data   struct {
					Cursors []struct {
						Row    json.Number `json:"row"`
						Column json.Number `json:"column"`
					} `json:"cursors"`
					Room string `json:"room"`
					User string `json:"user"`
				} `json:"data"`
			} `json:"rpc"`
		}
		if json.Unmarshal(raw, &cmd) != nil {
			return
		}
		switch {
		case cmd.Connect != nil:
			c.send(fmt.Sprintf(`{"id":%d,"connect":{"client":"x","ping":25,"pong":true}}`, cmd.ID))
		case cmd.Subscribe != nil:
			room := strings.TrimPrefix(cmd.Subscribe.Channel, "$shared_editor:room-")
			f.mu.Lock()
			open := f.rooms[room]
			if open {
				if f.subs[room] == nil {
					f.subs[room] = map[*cupsFakeConn]bool{}
				}
				f.subs[room][c] = true
			}
			f.mu.Unlock()
			if !open {
				c.send(fmt.Sprintf(`{"id":%d,"error":{"code":103,"message":"permission denied"}}`, cmd.ID))
				continue
			}
			c.send(fmt.Sprintf(`{"id":%d,"subscribe":{}}`, cmd.ID))
		case cmd.RPC != nil:
			c.send(fmt.Sprintf(`{"id":%d,"rpc":{}}`, cmd.ID))
			if d := cmd.RPC.Data; cmd.RPC.Method == "shared_editor_change_cursors" && len(d.Cursors) > 0 {
				cursors := make([]any, len(d.Cursors))
				ok := true
				for i, cur := range d.Cursors {
					row, errR := cur.Row.Int64()
					col, errC := cur.Column.Int64()
					if errR != nil || errC != nil {
						c.send(fmt.Sprintf(`{"id":%d,"error":{"code":400,"message":"column must be a number"}}`, cmd.ID))
						ok = false
						break
					}
					cursors[i] = map[string]any{"row": row, "column": col}
				}
				if ok {
					f.relay(d.Room, d.User, cursors)
				}
			}
		}
	}
}

// relay hands a cursor update to everyone in the room, the sender included,
// as Centrifugo does.
func (f *cupsFake) relay(room, user string, cursors []any) {
	push, _ := json.Marshal(map[string]any{"push": map[string]any{
		"channel": "$shared_editor:room-" + room,
		"pub": map[string]any{"data": map[string]any{
			"type":    "cursors_update",
			"payload": map[string]any{"user_uuid": user, "cursors": cursors},
		}},
	}})
	f.mu.Lock()
	var to []*cupsFakeConn
	for c := range f.subs[room] {
		to = append(to, c)
	}
	f.mu.Unlock()
	for _, c := range to {
		c.send(string(push))
	}
}

// jsCups loads the shipped cupsonline script, aimed at the fake server, and
// builds the transport around it.
func jsCups(t *testing.T, fake *cupsFake, url string, exit bool) *ScriptTransport {
	t.Helper()
	src, err := os.ReadFile("js/cupsonline.js")
	if err != nil {
		t.Fatal(err)
	}
	const base = `var BASE_ROOM_URL = "https://interview.cups.online/live-coding/";`
	if !bytes.Contains(src, []byte(base)) {
		t.Fatal("the script's base URL line changed; update this test")
	}
	patched := strings.Replace(string(src), base, `var BASE_ROOM_URL = "`+fake.srv.URL+`/live-coding/";`, 1)
	path, pub := mustSignedScript(t, patched)
	tr, err := New("cups", path, pub, url, map[string]interface{}{"exit": exit}, transport.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

// collect records what a transport receives.
type collector struct {
	mu   sync.Mutex
	got  map[string]int
	last time.Time
}

func newCollector() *collector { return &collector{got: map[string]int{}} }

func (c *collector) add(b []byte) {
	c.mu.Lock()
	c.got[string(b)]++
	c.last = time.Now()
	c.mu.Unlock()
}

// testPackets are distinct packets of mixed sizes: small ones that share a
// batch, and big ones that must be cut across messages and put back together.
func testPackets(n int, tag byte) [][]byte {
	sizes := []int{20, 64, 576, 1400, 4000, 9000, 20000}
	out := make([][]byte, n)
	for i := range out {
		sz := sizes[i%len(sizes)]
		p := make([]byte, sz)
		_, _ = rand.Read(p)
		p[0], p[1] = tag, byte(i)
		if sz > 2 {
			v, _ := rand.Int(rand.Reader, big.NewInt(255))
			p[2] = byte(v.Int64())
		}
		out[i] = p
	}
	return out
}

func waitConnected(t *testing.T, name string, tr transport.Transport, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if tr.IsConnected() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s never connected", name)
}

// deliver sends the packets and keeps re-sending the ones the other side has
// not got yet until all have arrived: the channel is pub/sub with no history,
// so a packet sent into a room the peer has not subscribed to yet is gone, as
// on the real service (the Session above retransmits; this test plays that
// part).
func deliver(t *testing.T, from transport.Transport, to *collector, pkts [][]byte, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		to.mu.Lock()
		var missing [][]byte
		for _, p := range pkts {
			if to.got[string(p)] == 0 {
				missing = append(missing, p)
			}
		}
		to.mu.Unlock()
		if len(missing) == 0 {
			return
		}
		if time.Now().After(deadline) {
			var sizes []int
			for _, p := range missing {
				sizes = append(sizes, len(p))
			}
			t.Fatalf("%d of %d packets never arrived (sizes %v)", len(missing), len(pkts), sizes)
		}
		for _, p := range missing {
			_ = from.Send(p)
		}
		time.Sleep(400 * time.Millisecond)
	}
}

// A JS exit that has no rooms creates them, reports them, and a native client
// given that list talks to it both ways.
func TestInteropCupsJSExitNativeClient(t *testing.T) {
	fake := newCupsFake(t)
	exit := jsCups(t, fake, "", true)
	listCh := make(chan string, 4)
	exit.OnRoomList(func(p string) { listCh <- p })
	exitGot := newCollector()
	exit.Receive(exitGot.add)
	if err := exit.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exit.Stop() })

	var list string
	select {
	case list = <-listCh:
	case <-time.After(20 * time.Second):
		t.Fatal("the JS exit never reported the rooms it created")
	}
	if list == "" || fake.created == 0 {
		t.Fatalf("room list %q, rooms created %d", list, fake.created)
	}
	if exit.RoomList() != list {
		t.Fatalf("RoomList() = %q, want %q", exit.RoomList(), list)
	}

	client := cupsonline.NewCupsonlineTransport(list, transport.DefaultConfig(), true)
	clientGot := newCollector()
	client.Receive(clientGot.add)
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Stop() })
	waitConnected(t, "native client", client, 20*time.Second)
	waitConnected(t, "JS exit", exit, 20*time.Second)

	up, down := testPackets(21, 'u'), testPackets(21, 'd')
	deliver(t, client, exitGot, up, 60*time.Second)
	deliver(t, exit, clientGot, down, 60*time.Second)
}

// A native exit's rooms, joined by a JS client. The JS transport speaks the
// unstriped wire, so the exit is asked for the old list (CUPS_STRIPE=0); a
// striped list is one the JS client must refuse
// (TestInteropCupsJSClientRefusesStripedList).
func TestInteropCupsNativeExitJSClient(t *testing.T) {
	fake := newCupsFake(t)
	t.Setenv("CUPS_STRIPE", "0")
	exit := cupsonline.NewCupsonlineTransport("", transport.DefaultConfig(), false)
	listCh := make(chan string, 4)
	exit.OnRoomList(func(p string) { listCh <- p })
	exitGot := newCollector()
	exit.Receive(exitGot.add)
	if err := exit.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exit.Stop() })
	var list string
	select {
	case list = <-listCh:
	case <-time.After(20 * time.Second):
		t.Fatal("the native exit never reported its rooms")
	}

	client := jsCups(t, fake, list, false)
	clientGot := newCollector()
	client.Receive(clientGot.add)
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Stop() })
	waitConnected(t, "native exit", exit, 20*time.Second)
	waitConnected(t, "JS client", client, 20*time.Second)

	up, down := testPackets(21, 'U'), testPackets(21, 'D')
	deliver(t, client, exitGot, up, 60*time.Second)
	deliver(t, exit, clientGot, down, 60*time.Second)
}

// A client never creates rooms, and with none to join it fails instead of
// waiting in rooms of its own.
func TestInteropCupsJSClientWithoutRoomsDoesNotCreateAny(t *testing.T) {
	fake := newCupsFake(t)
	client := jsCups(t, fake, "", false)
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Stop() })
	time.Sleep(1500 * time.Millisecond)
	if fake.created != 0 {
		t.Fatalf("a client created %d rooms", fake.created)
	}
	if state, _ := client.LastState(); state != "dead" {
		t.Fatalf("state %q, want dead", state)
	}
}

// A striped room list (a 0.7.1+ native exit) is not one the unstriped JS
// transport can speak: it must fail rather than join the rooms and send the
// exit frames it would misread.
func TestInteropCupsJSClientRefusesStripedList(t *testing.T) {
	fake := newCupsFake(t)
	list := transport.PackRooms([]string{"0b3c6d1e-1111-2222-3333-444455556666"}, true)
	client := jsCups(t, fake, list, false)
	_ = client.Start()
	t.Cleanup(func() { _ = client.Stop() })
	time.Sleep(1500 * time.Millisecond)
	if fake.created != 0 {
		t.Fatalf("a client created %d rooms", fake.created)
	}
	if state, _ := client.LastState(); state != "dead" {
		t.Fatalf("state %q, want dead", state)
	}
}

// Packets cut across messages stay with their sender: two members of one room
// sending at once must not take bytes from each other's stream.
func TestCupsJSReassemblesPerSender(t *testing.T) {
	src, err := os.ReadFile("js/cupsonline.js")
	if err != nil {
		t.Fatal(err)
	}
	vm := goja.New()
	registerInspectAPI(vm)
	if _, err := vm.RunString(string(src)); err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := vm.Set("emit", func(call goja.FunctionCall) goja.Value {
		var b []byte
		_ = vm.ExportTo(call.Argument(0), &b)
		got = append(got, string(b))
		return goja.Undefined()
	}); err != nil {
		t.Fatal(err)
	}
	// Room's constructor opens an HTTP session; here there is no network.
	httpStub := vm.NewObject()
	_ = httpStub.Set("newSession", func(goja.FunctionCall) goja.Value { return vm.NewObject() })
	if err := vm.Set("http", httpStub); err != nil {
		t.Fatal(err)
	}
	if _, err := vm.RunString(`var __room = new Room(0, "r"); __room.auth = { userUUID: "me" };`); err != nil {
		t.Fatal(err)
	}

	frame := func(sender string, chunk []byte) string {
		payload := make([]byte, 2+len(chunk))
		payload[0], payload[1] = byte(len(chunk)>>8), byte(len(chunk))
		copy(payload[2:], chunk)
		var nums []uint64
		for i := 0; i < len(payload); i += 6 {
			var v uint64
			for j := 0; j < 6; j++ {
				v <<= 8
				if i+j < len(payload) {
					v |= uint64(payload[i+j])
				}
			}
			nums = append(nums, v)
		}
		var cursors []map[string]any
		for i := 0; i < len(nums); i += 2 {
			c := map[string]any{"row": nums[i], "column": uint64(0)}
			if i+1 < len(nums) {
				c["column"] = nums[i+1]
			}
			cursors = append(cursors, c)
		}
		b, _ := json.Marshal(map[string]any{"push": map[string]any{"pub": map[string]any{"data": map[string]any{
			"type": "cursors_update", "payload": map[string]any{"user_uuid": sender, "cursors": cursors},
		}}}})
		return string(b)
	}

	// Each sender's packet (2-byte length + 10 bytes) is cut into two messages;
	// the two senders' messages interleave.
	pa := append([]byte{0, 10}, []byte("AAAAAAAAAA")...)
	pb := append([]byte{0, 10}, []byte("BBBBBBBBBB")...)
	feed := func(line string) {
		if _, err := vm.RunString(`__room.handleLine(` + mustJSONString(line) + `)`); err != nil {
			t.Fatal(err)
		}
	}
	feed(frame("alice", pa[:5]))
	feed(frame("bob", pb[:5]))
	feed(frame("alice", pa[5:]))
	feed(frame("bob", pb[5:]))
	if fmt.Sprint(got) != fmt.Sprint([]string{"AAAAAAAAAA", "BBBBBBBBBB"}) {
		t.Fatalf("delivered %q, want each member's own packet intact", got)
	}
	// our own echo is ignored
	feed(frame("me", pa))
	if len(got) != 2 {
		t.Fatalf("own echo was delivered: %q", got)
	}
}

func mustJSONString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
