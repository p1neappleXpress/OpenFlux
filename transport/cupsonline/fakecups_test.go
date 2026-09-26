package cupsonline

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"openflux/transport"
)

// fakeCups stands in for cups.online: room pages, subscription tokens and a
// Centrifugo that relays cursor updates to everyone in a room. The tests run
// an exit node and a client through it end to end, and break it on purpose.
type fakeCups struct {
	srv *httptest.Server

	mu        sync.Mutex
	users     map[string]string // session cookie -> user uuid
	rooms     map[string]bool   // room uuid -> still open
	subs      map[string]map[*fakeConn]bool
	conns     map[*fakeConn]bool
	pageHits  map[string]int
	failPages map[string]int // room -> page loads still to fail with 502
	failJoins bool           // every existing room's page fails with 502; creating still works
	// redirectGone makes a closed room's page hand out a brand-new room, the
	// way cups does, instead of a 404.
	redirectGone bool
	// pack sends a ping and the push in one frame, one per line.
	pack bool
	// stall makes page loads hang until it's closed, like a dead network.
	stall      chan struct{}
	created    int
	maxCursors int
}

type fakeConn struct {
	ws *websocket.Conn
	mu sync.Mutex
}

func (c *fakeConn) send(msg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ws.WriteMessage(websocket.TextMessage, []byte(msg))
}

func newFakeCups(t *testing.T) *fakeCups {
	f := &fakeCups{
		users:     map[string]string{},
		rooms:     map[string]bool{},
		subs:      map[string]map[*fakeConn]bool{},
		conns:     map[*fakeConn]bool{},
		pageHits:  map[string]int{},
		failPages: map[string]int{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/live-coding/", f.page)
	mux.HandleFunc("/sub/", f.subToken)
	mux.HandleFunc("/connection/websocket", f.centrifugo)
	f.srv = httptest.NewServer(mux)

	old := baseRoomURL
	baseRoomURL = f.srv.URL + "/live-coding/"
	t.Cleanup(func() {
		baseRoomURL = old
		f.mu.Lock()
		for c := range f.conns {
			c.ws.Close()
		}
		f.mu.Unlock()
		f.srv.Close()
	})
	return f
}

func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func (f *fakeCups) page(w http.ResponseWriter, r *http.Request) {
	var stall chan struct{}
	f.locked(func() { stall = f.stall })
	if stall != nil {
		select {
		case <-stall:
		case <-r.Context().Done():
		}
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	user := ""
	if c, err := r.Cookie("sessionid"); err == nil {
		user = f.users[c.Value]
	}
	if user == "" {
		session := newUUID()
		user = newUUID()
		f.users[session] = user
		http.SetCookie(w, &http.Cookie{Name: "sessionid", Value: session, Path: "/"})
	}
	http.SetCookie(w, &http.Cookie{Name: "csrftoken", Value: "csrf-" + user, Path: "/"})

	room := r.URL.Query().Get("room")
	if room != "" {
		f.pageHits[room]++
	}
	open, known := f.rooms[room]
	switch {
	case room != "" && (f.failJoins || f.failPages[room] > 0):
		f.failPages[room]--
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	case room == "" || (known && !open && f.redirectGone):
		room = newUUID()
		f.rooms[room] = true
		f.created++
	case !open:
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

func (f *fakeCups) subToken(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie("csrftoken")
	if r.Method != http.MethodPost || err != nil || r.Header.Get("X-CSRFToken") != c.Value {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var body struct {
		Channel string `json:"channel"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	json.NewEncoder(w).Encode(map[string]string{"token": "sub-" + body.Channel})
}

func (f *fakeCups) centrifugo(w http.ResponseWriter, r *http.Request) {
	ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := &fakeConn{ws: ws}
	f.mu.Lock()
	f.conns[c] = true
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.conns, c)
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
					f.subs[room] = map[*fakeConn]bool{}
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
				// Model the real server: a non-numeric row/column is rejected,
				// anything else is relayed verbatim and in order.
				cursors := make([]any, len(d.Cursors))
				for i, cur := range d.Cursors {
					row, errR := cur.Row.Int64()
					col, errC := cur.Column.Int64()
					if errR != nil || errC != nil {
						c.send(fmt.Sprintf(`{"id":%d,"error":{"code":400,"message":"column must be a number"}}`, cmd.ID))
						cursors = nil
						break
					}
					cursors[i] = map[string]any{"row": row, "column": col}
				}
				if cursors != nil {
					f.relay(d.Room, d.User, cursors)
				}
			}
		}
	}
}

// relay hands a cursor update to everyone in the room, the sender included,
// the way Centrifugo does.
func (f *fakeCups) relay(room, user string, cursors []any) {
	push, _ := json.Marshal(map[string]any{"push": map[string]any{
		"channel": "$shared_editor:room-" + room,
		"pub": map[string]any{"data": map[string]any{
			"type": "cursors_update",
			"payload": map[string]any{
				"user_uuid": user,
				"cursors":   cursors,
			},
		}},
	}})
	msg := string(push)

	f.mu.Lock()
	f.maxCursors = max(f.maxCursors, len(cursors))
	if f.pack {
		msg = "{}\n" + msg
	}
	var to []*fakeConn
	for c := range f.subs[room] {
		to = append(to, c)
	}
	f.mu.Unlock()
	for _, c := range to {
		c.send(msg)
	}
}

// closeRoom closes a room for good and hangs up on everyone in it.
func (f *fakeCups) closeRoom(room string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rooms[room] = false
	for c := range f.subs[room] {
		c.ws.Close()
	}
	delete(f.subs, room)
}

// dropConns hangs up on everyone in a room that stays open, like a network
// blip.
func (f *fakeCups) dropConns(room string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for c := range f.subs[room] {
		c.ws.Close()
	}
}

func (f *fakeCups) locked(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

func (f *fakeCups) hits(room string) (n int) {
	f.locked(func() { n = f.pageHits[room] })
	return
}

func (f *fakeCups) roomsCreated() (n int) {
	f.locked(func() { n = f.created })
	return
}

func (f *fakeCups) openConns() (n int) {
	f.locked(func() { n = len(f.conns) })
	return
}

// ---- helpers ----

func fastConfig() CupsonlineConfig {
	c := DefaultCupsonlineConfig()
	c.NumRooms = 3
	c.RoomCreatePause = time.Millisecond
	c.ReconnectMinDelay = 20 * time.Millisecond
	c.ReconnectMaxDelay = 200 * time.Millisecond
	c.RoomGoneRetryMin = 300 * time.Millisecond
	c.RoomGoneRetryMax = 300 * time.Millisecond
	c.WSHandshakeTimeout = 2 * time.Second
	c.StatsInterval = time.Hour
	c.SendInterval = 0 // no throttling against the in-process fake
	return c
}

type peer struct {
	*CupsonlineTransport
	got chan []byte
}

func startPeer(t *testing.T, url string, isClient bool, cfg CupsonlineConfig) (*peer, error) {
	tr := NewCupsonlineTransport(url, transport.DefaultConfig(), isClient)
	tr.config = cfg
	p := &peer{CupsonlineTransport: tr, got: make(chan []byte, 4096)}
	tr.Receive(func(b []byte) { p.got <- append([]byte(nil), b...) })
	t.Cleanup(func() { tr.Stop() })
	if err := tr.Start(); err != nil {
		return nil, err
	}
	return p, nil
}

func mustStart(t *testing.T, url string, isClient bool, cfg CupsonlineConfig) *peer {
	t.Helper()
	p, err := startPeer(t, url, isClient, cfg)
	if err != nil {
		t.Fatalf("start (client=%v): %v", isClient, err)
	}
	return p
}

// startPair brings up an exit node that creates the rooms and a client that
// joins them from the printed string, and waits until every channel is up.
func startPair(t *testing.T, cfg CupsonlineConfig) (exit, client *peer) {
	t.Helper()
	exit = mustStart(t, "", false, cfg)
	client = mustStart(t, packRooms(exit.RoomUUIDs()), true, cfg)
	waitFor(t, 5*time.Second, "all channels up", func() bool {
		return allConnected(exit) && allConnected(client)
	})
	return exit, client
}

func allConnected(p *peer) bool {
	for _, ws := range p.wss {
		if !ws.connected.Load() {
			return false
		}
	}
	return true
}

func roomDead(p *peer, room string) bool {
	for _, ws := range p.wss {
		if ws.roomUUID == room {
			return ws.dead.Load()
		}
	}
	return false
}

func waitFor(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// deliver keeps sending until a copy gets through: right after a room dies
// the first few can be lost, like packets on any link.
func deliver(t *testing.T, from, to *peer, payload []byte, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		from.Send(payload)
		wait := time.After(100 * time.Millisecond)
	drain:
		for {
			select {
			case got := <-to.got:
				if bytes.Equal(got, payload) {
					return
				}
			case <-wait:
				break drain
			}
		}
	}
	t.Fatalf("%q never got through", payload)
}

func next(t *testing.T, p *peer) []byte {
	t.Helper()
	select {
	case got := <-p.got:
		return got
	case <-time.After(5 * time.Second):
		t.Fatal("nothing arrived")
		return nil
	}
}

// drain discards anything already delivered to a peer, so one phase's
// leftovers don't bleed into the next.
func drain(p *peer) {
	for {
		select {
		case <-p.got:
		default:
			return
		}
	}
}

// expectSet reads len(want) payloads and checks they are exactly want as a
// multiset. Order isn't asserted: frames spread across rooms can arrive in a
// different order, and only that every one arrives intact and once matters.
func expectSet(t *testing.T, p *peer, want [][]byte) {
	t.Helper()
	remaining := make(map[string]int, len(want))
	for _, w := range want {
		remaining[string(w)]++
	}
	for range want {
		got := next(t, p)
		k := string(got)
		if remaining[k] == 0 {
			t.Fatalf("unexpected or duplicate payload of %d bytes", len(got))
		}
		remaining[k]--
	}
}

// roomsUsed counts how many of a peer's channels have sent at least one packet.
func roomsUsed(p *peer) (n int) {
	for _, ws := range p.wss {
		if ws.stats.packetsSent.Load() > 0 {
			n++
		}
	}
	return
}

// frame looks like what really reaches the transport: a codec frame (here
// the batched one, 0x02...), not an IP packet.
func frame(i int) []byte {
	return append([]byte{0x02, 0x01, byte(i >> 8), byte(i)}, bytes.Repeat([]byte{0x5a}, 100)...)
}

// ---- tests ----

func TestFramesCrossBothWays(t *testing.T) {
	newFakeCups(t)
	exit, client := startPair(t, fastConfig())

	var want [][]byte
	for i := 0; i < 100; i++ {
		f := frame(i)
		want = append(want, f)
		if err := client.Send(f); err != nil {
			t.Fatal(err)
		}
	}
	expectSet(t, exit, want)
	// Spreading is the point: 100 frames must not all funnel through one room.
	if used := roomsUsed(client); used < 2 {
		t.Fatalf("frames used only %d room(s), expected them spread", used)
	}

	exit.Send(frame(1000))
	if got := next(t, client); !bytes.Equal(got, frame(1000)) {
		t.Fatalf("reply: got %x", got[:4])
	}
}

// A room closes: the traffic it would have carried has to keep flowing through
// the rooms still up, both ways, instead of the tunnel going dark.
func TestTrafficSurvivesLosingARoom(t *testing.T) {
	f := newFakeCups(t)
	exit, client := startPair(t, fastConfig())
	deliver(t, client, exit, []byte("before-up"), 2*time.Second)
	deliver(t, exit, client, []byte("before-down"), 2*time.Second)

	gone := client.wss[0].roomUUID
	f.closeRoom(gone)
	waitFor(t, 5*time.Second, "the closed room reported on both sides", func() bool {
		return roomDead(client, gone) && roomDead(exit, gone)
	})

	// The surviving rooms still carry traffic both ways.
	deliver(t, client, exit, []byte("after-up"), 5*time.Second)
	deliver(t, exit, client, []byte("after-down"), 5*time.Second)
	if !client.IsConnected() || !exit.IsConnected() {
		t.Fatal("other rooms are up, the transport must still say connected")
	}
}

func TestClosedRoomIsProbedSlowly(t *testing.T) {
	f := newFakeCups(t)
	f.redirectGone = true // every probe makes cups hand out a new room
	cfg := fastConfig()
	cfg.RoomGoneRetryMin, cfg.RoomGoneRetryMax = 500*time.Millisecond, 500*time.Millisecond
	exit, client := startPair(t, cfg)

	room := client.wss[0].roomUUID
	f.closeRoom(room)
	waitFor(t, 5*time.Second, "room reported closed on both sides", func() bool {
		return roomDead(client, room) && roomDead(exit, room)
	})

	hits, created := f.hits(room), f.roomsCreated()
	time.Sleep(2 * time.Second)
	// Both peers probe it: at most 2 s / 500 ms + 1 each.
	if n := f.hits(room) - hits; n > 10 {
		t.Fatalf("closed room probed %d times in 2 s", n)
	}
	if n := f.roomsCreated() - created; n > 10 {
		t.Fatalf("%d rooms made by probing a closed one in 2 s", n)
	}
}

// A connection that drops comes back with the tokens it has. Loading the
// room page on every reconnect turns a flaky network into a flood of page
// loads, and that's how an IP gets rate-limited.
func TestDroppedConnectionReconnectsWithoutReloadingThePage(t *testing.T) {
	f := newFakeCups(t)
	exit, client := startPair(t, fastConfig())
	room := client.wss[0].roomUUID
	hits := f.hits(room)

	for i := 0; i < 5; i++ {
		f.dropConns(room)
		waitFor(t, 5*time.Second, "reconnect", func() bool {
			return client.wss[0].stats.reconnects.Load() > uint64(i) && allConnected(client) && allConnected(exit)
		})
	}
	if n := f.hits(room) - hits; n != 0 {
		t.Fatalf("room page loaded %d times over 5 dropped connections", n)
	}
	deliver(t, client, exit, []byte("still works"), 2*time.Second)
}

// A room the client couldn't enter at start still gets a channel and joins
// once it can. Dropping it left the exit node's traffic in it unheard.
func TestClientRetriesRoomItCouldNotEnterAtStart(t *testing.T) {
	f := newFakeCups(t)
	cfg := fastConfig()
	exit := mustStart(t, "", false, cfg)
	ids := exit.RoomUUIDs()
	f.locked(func() { f.failPages[ids[1]] = 3 })

	client := mustStart(t, packRooms(ids), true, cfg)
	if len(client.wss) != len(ids) {
		t.Fatalf("client keeps %d of %d rooms", len(client.wss), len(ids))
	}
	waitFor(t, 5*time.Second, "late room joined", func() bool { return client.wss[1].connected.Load() })
	waitFor(t, 5*time.Second, "exit node up", func() bool { return allConnected(exit) })

	// The late room must carry its share once it joins: send enough that the
	// round-robin certainly uses it, and check it did and everything arrived.
	before := exit.wss[1].stats.packetsSent.Load()
	var want [][]byte
	for i := 0; i < 12; i++ {
		p := []byte{byte(i), 0xaa}
		want = append(want, p)
		if err := exit.Send(p); err != nil {
			t.Fatal(err)
		}
	}
	expectSet(t, client, want)
	if exit.wss[1].stats.packetsSent.Load() == before {
		t.Fatal("the late room carried no traffic")
	}
}

func TestExitKeepsSavedRoomsThroughNetworkTrouble(t *testing.T) {
	f := newFakeCups(t)
	cfg := fastConfig()
	first := mustStart(t, "", false, cfg)
	ids := first.RoomUUIDs()
	packed := packRooms(ids)
	first.Stop()
	created := f.roomsCreated()

	f.locked(func() { f.failJoins = true })
	if _, err := startPeer(t, packed, false, cfg); err == nil {
		t.Fatal("exit node must refuse to start while its rooms don't answer")
	}
	if f.roomsCreated() != created {
		t.Fatal("new rooms made over a network error: the phone's string is lost")
	}

	f.locked(func() { f.failJoins = false })
	again := mustStart(t, packed, false, cfg)
	if strings.Join(again.RoomUUIDs(), ",") != strings.Join(ids, ",") || f.roomsCreated() != created {
		t.Fatal("restart with the saved string must reuse the same rooms")
	}
	again.Stop()

	for _, id := range ids {
		f.closeRoom(id)
	}
	fresh := mustStart(t, packed, false, cfg)
	if f.roomsCreated() != created+cfg.NumRooms {
		t.Fatalf("all saved rooms gone: want %d new rooms, got %d", cfg.NumRooms, f.roomsCreated()-created)
	}
	for _, id := range fresh.RoomUUIDs() {
		if strings.Contains(packed, id) {
			t.Fatal("reused a closed room")
		}
	}
}

func TestClientWithoutReachableRoomsFails(t *testing.T) {
	f := newFakeCups(t)
	exit := mustStart(t, "", false, fastConfig())
	packed := packRooms(exit.RoomUUIDs())
	f.locked(func() { f.failJoins = true })
	if _, err := startPeer(t, packed, true, fastConfig()); err == nil {
		t.Fatal("client with no room to enter must not report success")
	}
}

func TestBatchesStayUnderCursorLimit(t *testing.T) {
	f := newFakeCups(t)
	cfg := fastConfig()
	exit, client := startPair(t, cfg)

	body := bytes.Repeat([]byte{0xab}, 7000)
	var want [][]byte
	for i := 0; i < 200; i++ {
		p := append([]byte{byte(i)}, body...)
		want = append(want, p)
		if err := client.Send(p); err != nil {
			t.Fatal(err)
		}
	}
	expectSet(t, exit, want)
	f.locked(func() {
		if f.maxCursors > cfg.MaxCursors {
			t.Fatalf("%d cursors in one message, limit %d", f.maxCursors, cfg.MaxCursors)
		}
	})
	if err := client.Send(make([]byte, cfg.MaxPayloadBytes+1)); err == nil {
		t.Fatal("a payload that can't fit in one message must be refused, not sent")
	}
}

// A packet larger than one message must be split on send and stitched back
// on receive, in order and intact. This is the throughput path that broke
// against the real server when a whole batch went in one oversized message.
func TestLargePacketsSplitAndReassemble(t *testing.T) {
	f := newFakeCups(t)
	cfg := fastConfig()
	exit, client := startPair(t, cfg)

	// Each packet spans several messages; distinct contents so a swap shows.
	const n = 40
	sizes := []int{cfg.MaxMessageData - 5, cfg.MaxMessageData, cfg.MaxMessageData + 5, 3*cfg.MaxMessageData + 1, 8192}
	want := make([][]byte, n)
	for i := 0; i < n; i++ {
		size := sizes[i%len(sizes)]
		p := make([]byte, size)
		for j := range p {
			p[j] = byte(i*31 + j)
		}
		want[i] = p
		if err := client.Send(p); err != nil {
			t.Fatalf("packet %d (%d bytes): %v", i, size, err)
		}
	}
	// Each packet reassembles within whichever room carried it; across rooms
	// they may arrive in a different order, so check the set, not the order.
	expectSet(t, exit, want)
	f.locked(func() {
		if f.maxCursors > cfg.MaxCursors {
			t.Fatalf("%d cursors in one message, over the %d limit", f.maxCursors, cfg.MaxCursors)
		}
	})
}

func TestNumberPackingRoundTrips(t *testing.T) {
	for _, n := range []int{0, 1, 2, 5, 6, 7, 11, 12, 13, 100, 4096} {
		in := make([]byte, n)
		for i := range in {
			in[i] = byte(i*7 + 1) // never all-zero, so nothing looks like padding
		}
		out := bytesFromNumbers(packNumbers(in))
		if len(out) < len(in) || !bytes.Equal(out[:len(in)], in) {
			t.Fatalf("len %d did not round-trip", n)
		}
		// padding is only trailing zeros
		for _, b := range out[len(in):] {
			if b != 0 {
				t.Fatalf("len %d: non-zero padding", n)
			}
		}
	}
	// values must stay inside what cups keeps exact (< 2^53).
	for _, v := range packNumbers(bytes.Repeat([]byte{0xff}, 4096)) {
		if v >= 1<<(8*bytesPerNumber) {
			t.Fatalf("number %d exceeds %d bytes", v, bytesPerNumber)
		}
	}
}

// Centrifugo may pack several messages into one frame; parsing it whole
// used to lose every packet in it.
func TestPackedFramesAreTakenApart(t *testing.T) {
	f := newFakeCups(t)
	f.pack = true
	exit, client := startPair(t, fastConfig())
	deliver(t, client, exit, []byte("up"), 2*time.Second)
	deliver(t, exit, client, []byte("down"), 2*time.Second)
}

func TestStopHangsUpRightAway(t *testing.T) {
	f := newFakeCups(t)
	_, client := startPair(t, fastConfig())
	before := f.openConns()

	client.Stop()
	waitFor(t, time.Second, "client sockets closed", func() bool {
		return f.openConns() == before-len(client.wss)
	})
	if client.IsConnected() {
		t.Fatal("stopped transport says connected")
	}
	if err := client.Send([]byte("x")); err == nil {
		t.Fatal("send after stop must fail")
	}
	client.Stop() // twice must be harmless
}

func TestStopDuringStartAbortsIt(t *testing.T) {
	f := newFakeCups(t)
	exit := mustStart(t, "", false, fastConfig())
	packed := packRooms(exit.RoomUUIDs())

	stall := make(chan struct{})
	defer close(stall)
	f.locked(func() { f.stall = stall })

	tr := NewCupsonlineTransport(packed, transport.DefaultConfig(), true)
	tr.config = fastConfig()
	done := make(chan error, 1)
	go func() { done <- tr.Start() }()
	time.Sleep(100 * time.Millisecond)
	tr.Stop()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Start after Stop must not report success")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop didn't cut Start short")
	}
}

// gorilla allocates the socket buffers whole for every connection: at the
// old 32 MB each, four rooms took 256 MB on the phone, again on every
// reconnect.
func TestChannelsDontHoardMemory(t *testing.T) {
	newFakeCups(t)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	exit, client := startPair(t, fastConfig())
	runtime.ReadMemStats(&after)
	conns := len(exit.wss) + len(client.wss)
	if grew := int64(after.HeapAlloc) - int64(before.HeapAlloc); grew > 64<<20 {
		t.Fatalf("%d channels took %d MB of heap", conns, grew>>20)
	}
}

func TestPickRoomSpreadsAndSkipsDown(t *testing.T) {
	tr := &CupsonlineTransport{}
	for i := 0; i < 3; i++ {
		ws := &cupsWS{idx: i, roomUUID: fmt.Sprintf("room-%d-0000", i)}
		ws.connected.Store(true)
		tr.wss = append(tr.wss, ws)
	}

	// Round-robin visits every connected room.
	seen := map[*cupsWS]bool{}
	for i := 0; i < 30; i++ {
		seen[tr.pickRoom()] = true
	}
	if len(seen) != 3 {
		t.Fatalf("round-robin used %d of 3 rooms", len(seen))
	}

	// A disconnected room is skipped.
	tr.wss[1].connected.Store(false)
	for i := 0; i < 30; i++ {
		if tr.pickRoom() == tr.wss[1] {
			t.Fatal("pickRoom returned a disconnected room")
		}
	}

	// None up -> nil.
	for _, ws := range tr.wss {
		ws.connected.Store(false)
	}
	if tr.pickRoom() != nil {
		t.Fatal("no room up: pickRoom must return nil")
	}
}
