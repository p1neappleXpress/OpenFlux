package cupsonline

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/p1neappleXpress/OpenFlux/netbind"
	"github.com/p1neappleXpress/OpenFlux/transport"
)

// TestLiveRelayBench measures what cups.online really relays: sender
// connections post numbered messages, one receiver per room counts what
// arrives (unique seqs, bytes, latency) next to what the server answered the
// senders' RPCs with. Nothing here is a send-side figure.
//
//	OPENFLUX_CUPS_BENCH=1 go test -run TestLiveRelayBench -v -timeout 30m ./transport/cupsonline/
//
// Knobs: CB_ROOMS (1), CB_SENDERS per room (1), CB_SAMEUSER=1 (the senders of
// a room share one user), CB_SECS per phase (20), CB_ROOMLIST (a packed list
// to reuse instead of creating rooms), CB_PHASES (comma list, default
// "w1:8192"): w<N>:<bytes> keeps N RPCs in flight per connection, i<ms>:<bytes>
// sends every ms milliseconds whatever the replies.
func TestLiveRelayBench(t *testing.T) {
	if os.Getenv("OPENFLUX_CUPS_BENCH") == "" {
		t.Skip("set OPENFLUX_CUPS_BENCH=1 to run against the real cups.online")
	}
	nRooms := benchEnvInt("CB_ROOMS", 1)
	nSenders := benchEnvInt("CB_SENDERS", 1)
	secs := benchEnvInt("CB_SECS", 20)
	sameUser := os.Getenv("CB_SAMEUSER") != ""
	phases := strings.Split(benchEnv("CB_PHASES", "w1:8192"), ",")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var rooms []string
	var recvAuth []*cupsAuth
	if l := os.Getenv("CB_ROOMLIST"); l != "" {
		ids, _, err := transport.UnpackRooms(l)
		if err != nil {
			t.Fatal(err)
		}
		rooms = ids[:min(nRooms, len(ids))]
		for _, id := range rooms {
			a, err := joinRoom(ctx, id, nil)
			if err != nil {
				t.Fatalf("join %s: %v", id, err)
			}
			recvAuth = append(recvAuth, a)
		}
	} else {
		auths, err := createRooms(ctx, baseRoomURL, nRooms, 500*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range auths {
			rooms = append(rooms, a.roomUUID)
			recvAuth = append(recvAuth, a)
		}
		t.Logf("rooms: %s", transport.PackRooms(rooms, true))
	}

	st := &benchStats{}
	var receivers []*benchConn
	for i, a := range recvAuth {
		c, err := dialBench(ctx, a, st, -1)
		if err != nil {
			t.Fatalf("receiver %d: %v", i, err)
		}
		receivers = append(receivers, c)
	}
	var senders []*benchConn
	for r, id := range rooms {
		var shared *http.Client
		var reuse *cupsAuth
		for s := 0; s < nSenders; s++ {
			a := reuse
			if a == nil {
				var err error
				a, err = joinRoom(ctx, id, shared)
				if err != nil {
					t.Fatalf("sender %d/%d join: %v", r, s, err)
				}
			}
			if os.Getenv("CB_SAMEUSER") == "2" {
				reuse = a // one token for every connection
			}
			if sameUser {
				shared = a.httpClient
			}
			c, err := dialBench(ctx, a, st, len(senders))
			if err != nil {
				t.Fatalf("sender %d/%d dial: %v", r, s, err)
			}
			senders = append(senders, c)
		}
	}
	t.Logf("%d rooms x %d senders (same user: %v), receivers: %d, %ds per phase", len(rooms), nSenders, sameUser, len(receivers), secs)
	time.Sleep(2 * time.Second)

	for pi, ph := range phases {
		mode, size := parsePhase(t, ph)
		st.reset(len(senders), byte(pi))
		start := time.Now()
		end := start.Add(time.Duration(secs) * time.Second)
		var wg sync.WaitGroup
		for _, s := range senders {
			wg.Add(1)
			go func(s *benchConn) {
				defer wg.Done()
				s.sendPhase(byte(pi), mode, size, end)
			}(s)
		}
		wg.Wait()
		time.Sleep(4 * time.Second) // stragglers
		t.Logf("=== phase %s (%ds) ===", ph, secs)
		st.report(t, senders, float64(secs))
		time.Sleep(2 * time.Second)
	}
	for _, c := range append(receivers, senders...) {
		c.conn.Close()
	}
}

func benchEnv(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func benchEnvInt(name string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(name)); err == nil && n > 0 {
		return n
	}
	return def
}

type benchMode struct {
	window   int
	interval time.Duration
}

func parsePhase(t *testing.T, ph string) (benchMode, int) {
	parts := strings.SplitN(ph, ":", 2)
	if len(parts) != 2 || len(parts[0]) < 2 {
		t.Fatalf("bad phase %q", ph)
	}
	n, err1 := strconv.Atoi(parts[0][1:])
	size, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || n <= 0 || size < 20 {
		t.Fatalf("bad phase %q", ph)
	}
	switch parts[0][0] {
	case 'w':
		return benchMode{window: n}, size
	case 'i':
		return benchMode{interval: time.Duration(n) * time.Millisecond}, size
	}
	t.Fatalf("bad phase %q", ph)
	return benchMode{}, 0
}

// benchStats is one phase's tally, shared by all connections.
type benchStats struct {
	mu      sync.Mutex
	phase   byte
	sent    []int
	ok      []int
	errs    []int
	timeout []int
	rpcLat  [][]time.Duration
	seen    []map[uint32]bool
	bytes   []int
	dup     []int
	lat     [][]time.Duration
	errText map[string]int
}

func (s *benchStats) reset(n int, phase byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.phase = phase
	s.sent, s.ok, s.errs, s.timeout = make([]int, n), make([]int, n), make([]int, n), make([]int, n)
	s.rpcLat, s.lat = make([][]time.Duration, n), make([][]time.Duration, n)
	s.seen, s.bytes, s.dup = make([]map[uint32]bool, n), make([]int, n), make([]int, n)
	for i := range s.seen {
		s.seen[i] = map[uint32]bool{}
	}
	s.errText = map[string]int{}
}

func pct(d []time.Duration, p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	c := append([]time.Duration(nil), d...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	return c[min(int(float64(len(c))*p), len(c)-1)].Round(time.Millisecond)
}

func (s *benchStats) report(t *testing.T, senders []*benchConn, secs float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var tSent, tOK, tErr, tTO, tRecv, tBytes int
	for i := range s.sent {
		recv := len(s.seen[i])
		loss := 0.0
		if s.sent[i] > 0 {
			loss = 100 * (1 - float64(recv)/float64(s.sent[i]))
		}
		t.Logf("  sender %d (room %s user %s): sent %d (%.1f/s) rpc ok %d err %d timeout %d rpc p50 %v p95 %v | delivered %d (%.1f/s) dup %d loss %.1f%% %.1f KB/s lat p50 %v p95 %v",
			i, shortRoom(senders[i].auth.roomUUID), shortRoom(senders[i].auth.userUUID), s.sent[i], float64(s.sent[i])/secs, s.ok[i], s.errs[i], s.timeout[i],
			pct(s.rpcLat[i], .5), pct(s.rpcLat[i], .95), recv, float64(recv)/secs, s.dup[i], loss, float64(s.bytes[i])/secs/1024,
			pct(s.lat[i], .5), pct(s.lat[i], .95))
		tSent += s.sent[i]
		tOK += s.ok[i]
		tErr += s.errs[i]
		tTO += s.timeout[i]
		tRecv += recv
		tBytes += s.bytes[i]
	}
	loss := 0.0
	if tSent > 0 {
		loss = 100 * (1 - float64(tRecv)/float64(tSent))
	}
	t.Logf("  TOTAL: sent %d ok %d err %d timeout %d | delivered %d loss %.1f%% = %.1f KB/s delivered", tSent, tOK, tErr, tTO, tRecv, loss, float64(tBytes)/secs/1024)
	for k, v := range s.errText {
		t.Logf("  error x%d: %s", v, k)
	}
}

// benchConn is one participant's socket.
type benchConn struct {
	auth    *cupsAuth
	conn    *websocket.Conn
	st      *benchStats
	idx     int // sender index, -1 for a receiver
	writeMu sync.Mutex
	id      atomic.Int64

	pendMu  sync.Mutex
	pending map[int64]time.Time
	slots   chan struct{}
}

const benchMagic = 0xCB

func dialBench(ctx context.Context, a *cupsAuth, st *benchStats, idx int) (*benchConn, error) {
	wsURL := strings.Replace(a.connURL, "https://", "wss://", 1)
	wsURL = strings.Replace(wsURL, "http://", "ws://", 1)
	wsURL = strings.TrimRight(wsURL, "/") + "/websocket"
	header := http.Header{}
	header.Set("Origin", originOf(a.connURL))
	header.Set("User-Agent", cupsUA)
	for _, c := range a.httpClient.Jar.Cookies(mustParseURL(a.connURL)) {
		header.Add("Cookie", c.Name+"="+c.Value)
	}
	d := websocket.Dialer{NetDialContext: netbind.DialContext, HandshakeTimeout: 15 * time.Second,
		ReadBufferSize: 64 << 10, WriteBufferSize: 64 << 10, EnableCompression: os.Getenv("CB_COMPRESS") == "1"}
	conn, resp, err := d.DialContext(ctx, wsURL, header)
	if err != nil {
		return nil, err
	}
	if d.EnableCompression && idx <= 0 {
		fmt.Printf("conn %d: Sec-WebSocket-Extensions=%q\n", idx, resp.Header.Get("Sec-WebSocket-Extensions"))
	}
	conn.EnableWriteCompression(d.EnableCompression)
	conn.SetReadLimit(8 << 20)
	c := &benchConn{auth: a, conn: conn, st: st, idx: idx, pending: map[int64]time.Time{}}
	c.id.Store(2)
	if err := c.write(map[string]interface{}{"id": 1, "connect": map[string]interface{}{"token": a.connToken, "name": "js"}}); err != nil {
		return nil, err
	}
	if err := readReply(conn, 15*time.Second, 1, "connect", nil); err != nil {
		return nil, err
	}
	if idx < 0 || os.Getenv("CB_NOSUB") != "1" {
		if err := c.write(map[string]interface{}{"id": 2, "subscribe": map[string]interface{}{"channel": a.channel, "token": a.subToken}}); err != nil {
			return nil, err
		}
		if err := readReply(conn, 15*time.Second, 2, "subscribe", nil); err != nil {
			return nil, err
		}
	}
	go c.readLoop()
	return c, nil
}

func (c *benchConn) write(v interface{}) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
	return c.conn.WriteMessage(websocket.TextMessage, b)
}

func (c *benchConn) readLoop() {
	type frame struct {
		raw []byte
		at  time.Time
	}
	q := make(chan frame, 100000)
	defer close(q)
	go func() {
		for f := range q {
			if os.Getenv("CB_DUMP") != "" {
				lines := splitFrame(f.raw)
				for _, l := range lines {
					head := string(l)
					if len(head) > 260 {
						head = head[:260]
					}
					fmt.Printf("RX frame %dB lines=%d line %dB: %s\n", len(f.raw), len(lines), len(l), head)
				}
			}
			for _, line := range splitFrame(f.raw) {
				if isPing(line) {
					c.writeMu.Lock()
					c.conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
					_ = c.conn.WriteMessage(websocket.TextMessage, []byte("{}"))
					c.writeMu.Unlock()
					continue
				}
				c.handle(line, f.at)
			}
		}
	}()
	var rbytes int64
	start := time.Now()
	defer func() {
		if c.idx < 0 {
			fmt.Printf("conn %d read %.1f MB in %s = %.0f KB/s on the wire\n", c.idx, float64(rbytes)/1e6, time.Since(start).Round(time.Second), float64(rbytes)/1024/time.Since(start).Seconds())
		}
	}()
	for {
		c.conn.SetReadDeadline(time.Now().Add(120 * time.Second))
		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			fmt.Printf("%s conn %d (room %s) closed: %v\n", time.Now().Format("15:04:05.000"), c.idx, shortRoom(c.auth.roomUUID), err)
			return
		}
		now := time.Now()
		rbytes += int64(len(raw))
		if c.idx < 0 && string(raw) != "{}" {
			q <- frame{raw, now}
			continue
		}
		for _, line := range splitFrame(raw) {
			if isPing(line) {
				c.writeMu.Lock()
				c.conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
				err := c.conn.WriteMessage(websocket.TextMessage, []byte("{}"))
				c.writeMu.Unlock()
				if os.Getenv("CB_PINGLOG") != "" {
					fmt.Printf("%s conn %d ping (frame %d lines, %d B) pong err=%v\n", now.Format("15:04:05.000"), c.idx, len(splitFrame(raw)), len(raw), err)
				}
				continue
			}
			c.handle(line, now)
		}
	}
}

func (c *benchConn) handle(line []byte, now time.Time) {
	var m struct {
		ID    int64           `json:"id"`
		Error json.RawMessage `json:"error"`
		Push  *struct {
			Pub struct {
				Data struct {
					Type    string `json:"type"`
					Payload struct {
						User    string `json:"user_uuid"`
						Cursors []struct {
							Row    float64 `json:"row"`
							Column float64 `json:"column"`
						} `json:"cursors"`
					} `json:"payload"`
				} `json:"data"`
			} `json:"pub"`
		} `json:"push"`
	}
	if json.Unmarshal(line, &m) != nil {
		return
	}
	if m.ID > 2 && c.idx >= 0 {
		c.pendMu.Lock()
		at, ok := c.pending[m.ID]
		delete(c.pending, m.ID)
		c.pendMu.Unlock()
		if !ok {
			return // timed out already
		}
		c.st.mu.Lock()
		if len(m.Error) > 0 {
			c.st.errs[c.idx]++
			c.st.errText[string(m.Error)]++
		} else {
			c.st.ok[c.idx]++
		}
		c.st.rpcLat[c.idx] = append(c.st.rpcLat[c.idx], now.Sub(at))
		c.st.mu.Unlock()
		if c.slots != nil {
			select {
			case <-c.slots:
			default:
			}
		}
		return
	}
	if m.Push == nil || c.idx >= 0 || m.Push.Pub.Data.Type != "cursors_update" {
		return
	}
	cur := m.Push.Pub.Data.Payload.Cursors
	nums := make([]uint64, 0, 2*len(cur))
	for _, x := range cur {
		nums = append(nums, uint64(x.Row), uint64(x.Column))
	}
	b := bytesFromNumbers(nums)
	if len(b) < 2 {
		return
	}
	n := int(binary.BigEndian.Uint16(b))
	if 2+n > len(b) || n < 16 {
		return
	}
	p := b[2 : 2+n]
	if p[0] != benchMagic {
		return
	}
	sender, phase, seq := int(binary.BigEndian.Uint16(p[1:3])), p[3], binary.BigEndian.Uint32(p[4:8])
	ts := time.Unix(0, int64(binary.BigEndian.Uint64(p[8:16])))
	s := c.st
	s.mu.Lock()
	defer s.mu.Unlock()
	if phase != s.phase || sender >= len(s.seen) {
		return
	}
	if s.seen[sender][seq] {
		s.dup[sender]++
		return
	}
	s.seen[sender][seq] = true
	s.bytes[sender] += n
	s.lat[sender] = append(s.lat[sender], now.Sub(ts))
}

func (c *benchConn) sendPhase(phase byte, mode benchMode, size int, end time.Time) {
	c.pendMu.Lock()
	c.pending = map[int64]time.Time{}
	c.pendMu.Unlock()
	if mode.window > 0 {
		c.slots = make(chan struct{}, mode.window)
	} else {
		c.slots = nil
	}
	body := make([]byte, size)
	for i := range body {
		body[i] = byte(i*7 + 1)
	}
	next := time.Now()
	for seq := uint32(0); time.Now().Before(end); seq++ {
		if mode.window > 0 {
			for {
				c.reap()
				select {
				case c.slots <- struct{}{}:
				case <-time.After(100 * time.Millisecond):
					if time.Now().Before(end) {
						continue
					}
					return
				}
				break
			}
		} else {
			if d := time.Until(next); d > 0 {
				time.Sleep(d)
			}
			next = next.Add(mode.interval)
			c.reap()
		}
		body[0] = benchMagic
		binary.BigEndian.PutUint16(body[1:3], uint16(c.idx))
		body[3] = phase
		binary.BigEndian.PutUint32(body[4:8], seq)
		binary.BigEndian.PutUint64(body[8:16], uint64(time.Now().UnixNano()))
		payload := make([]byte, 2+size)
		binary.BigEndian.PutUint16(payload, uint16(size))
		copy(payload[2:], body)
		id := c.id.Add(1)
		msg := map[string]interface{}{
			"rpc": map[string]interface{}{
				"method": "shared_editor_change_cursors",
				"data": map[string]interface{}{
					"cursors": cursorsFromBytes(payload), "ranges": []interface{}{},
					"room": c.auth.roomUUID, "user": c.auth.userUUID,
				},
			},
			"id": id,
		}
		c.pendMu.Lock()
		c.pending[id] = time.Now()
		c.pendMu.Unlock()
		if err := c.write(msg); err != nil {
			fmt.Println("write:", err)
			return
		}
		c.st.mu.Lock()
		c.st.sent[c.idx]++
		c.st.mu.Unlock()
	}
}

// reap counts RPCs unanswered for 10 s as timed out and frees their slots.
func (c *benchConn) reap() {
	now := time.Now()
	c.pendMu.Lock()
	var gone int
	for id, at := range c.pending {
		if now.Sub(at) > 10*time.Second {
			delete(c.pending, id)
			gone++
		}
	}
	c.pendMu.Unlock()
	if gone == 0 {
		return
	}
	c.st.mu.Lock()
	c.st.timeout[c.idx] += gone
	c.st.mu.Unlock()
	for ; gone > 0 && c.slots != nil; gone-- {
		select {
		case <-c.slots:
		default:
		}
	}
}
