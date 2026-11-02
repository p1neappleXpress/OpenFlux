package registry

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport"
	"github.com/p1neappleXpress/OpenFlux/transport/cupsonline"
	"github.com/p1neappleXpress/OpenFlux/transport/yandexcbir"
)

// cupsPointers carries cbirIds over the cups rooms: one short message per
// chunk, tagged so a peer ignores its own echo.
// cupsByURL holds the cups transports the session actually runs, by room
// list: a standalone cups instance connects but never transmits, because the
// write path belongs to the striped session lane. So the cbir transport rides
// the instance that already carries the session, not one of its own.
var (
	cupsMu    sync.Mutex
	cupsByURL = map[string]*cupsonline.CupsonlineTransport{}
)

func canonOfIDs(ids []string) string {
	seen := map[string]bool{}
	var out []string
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func canonOfList(list string) string {
	set := roomSet(list)
	if len(set) == 0 {
		return ""
	}
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	return canonOfIDs(ids)
}

func rememberCups(url string, c *cupsonline.CupsonlineTransport) {
	key := strings.TrimSpace(url)
	// An exit that minted its own rooms has no url to key on: take the set of
	// rooms the transport actually carries, or the pointer channel never binds.
	canon := canonOfList(key)
	if canon == "" && c != nil {
		if ids := c.RoomIDs(); len(ids) > 0 {
			canon = canonOfIDs(ids)
		}
	}
	cupsMu.Lock()
	cupsByURL[key] = c
	if canon != "" {
		cupsByURL[canon] = c
	}
	cupsMu.Unlock()
	cupsMu.Lock()
	cupsByURL[key] = c
	cupsMu.Unlock()
	if os.Getenv("OPENFLUX_CBIR_DEBUG") != "" {
		log.Printf("[cbir] session cups registered: %d rooms, %.28s…", len(roomSet(key)), key)
	}
}

// lookupCups finds the session's cups transport for this room list. The
// strings can differ in spelling (a phone and a node pack the same rooms
// differently), so the room SET decides, not the text.
func lookupCups(url string) *cupsonline.CupsonlineTransport {
	key := strings.TrimSpace(url)
	cupsMu.Lock()
	defer cupsMu.Unlock()
	if c := cupsByURL[key]; c != nil {
		return c
	}
	want := canonOfList(key)
	if want == "" {
		return nil
	}
	if c := cupsByURL[want]; c != nil {
		return c
	}
	wantSet := roomSet(key)
	for registered, c := range cupsByURL {
		if roomSetsEqual(wantSet, roomSet(registered)) || registered == want {
			return c
		}
	}
	return nil
}

// roomSet decodes a packed room list into its ids. The list travels
// base64-packed (that is what a link and the cups url carry), so the JSON is
// one decode further in.
func roomSet(list string) map[string]bool {
	raw := []byte(strings.TrimSpace(list))
	var packed struct {
		Rooms []string `json:"rooms"`
	}
	if err := json.Unmarshal(raw, &packed); err != nil {
		for _, enc := range []*base64.Encoding{base64.RawURLEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.StdEncoding} {
			if dec, derr := enc.DecodeString(strings.TrimSpace(list)); derr == nil {
				if json.Unmarshal(dec, &packed) == nil {
					break
				}
			}
		}
	}
	out := make(map[string]bool, len(packed.Rooms))
	for _, r := range packed.Rooms {
		out[r] = true
	}
	return out
}

func roomSetsEqual(a, b map[string]bool) bool {
	if len(a) == 0 || len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// pointerBatch is how often queued ids leave in one message.
const pointerBatch = 50 * time.Millisecond

type cupsPointers struct {
	cups     *cupsonline.CupsonlineTransport
	tr       *yandexcbir.Transport
	tag      string
	roomHint string

	mu      sync.Mutex
	pending []string
	ready   bool
	shared  bool
	rooms   string
}

// newSharedPointers rides the session's own cups transport: start is done by
// the session, so only the receive filter is installed here.
func newSharedPointers(cups *cupsonline.CupsonlineTransport, tr *yandexcbir.Transport) *cupsPointers {
	p := newCupsPointers(cups, tr)
	p.shared = true
	return p
}

func newCupsPointers(cups *cupsonline.CupsonlineTransport, tr *yandexcbir.Transport) *cupsPointers {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return &cupsPointers{cups: cups, tr: tr, tag: hex.EncodeToString(b[:])}
}

func (p *cupsPointers) Start() error                    { return nil }
func (p *cupsPointers) Stop() error                     { return p.cups.Stop() }
func (p *cupsPointers) IsConnected() bool               { return p.cups.IsConnected() }
func (p *cupsPointers) Stats() transport.TransportStats { return p.cups.Stats() }
func (p *cupsPointers) Receive(func([]byte))            {}

// Send queues one cbirId. The ids go out in one message every
// pointerBatch: cups throttles a room that is written to hundreds of times a
// minute (429 "превышено допустимое число отправок"), and one message per
// chunk was exactly that.
func (p *cupsPointers) Send(id string) error {
	p.mu.Lock()
	p.pending = append(p.pending, id)
	p.mu.Unlock()
	return nil
}

// SendControl ships one small frame over the rooms instead of a chunk: an ack
// must not cost an image upload, or the peer's TCP measures that upload as its
// round trip and never opens the window.
func (p *cupsPointers) SendControl(frame []byte) error {
	return p.cups.Send([]byte("OFXCTRL1:" + p.tag + ":" + base64.StdEncoding.EncodeToString(frame)))
}

// flush sends everything queued in one message.
// adopt moves the channel onto the session's own cups transport as soon as
// one is registered: a standalone cups connects but never transmits, so until
// this succeeds every id is queued, never sent into the void.
func (p *cupsPointers) adopt() bool {
	p.mu.Lock()
	own := p.cups
	shared := p.shared
	p.mu.Unlock()
	if own != nil && !shared {
		// This channel has a cups transport of its own: the session's instance
		// is wrapped, and a tap on it hears nothing, so do not switch back.
		return true
	}
	if p.cups == nil {
		cupsMu.Lock()
		for _, c := range cupsByURL {
			// RoomIDs is the list the transport was built with; JoinedRooms is
			// the proof it is actually inside a room. An exit that minted its
			// own rooms only ever has the second one.
			if len(c.RoomIDs()) > 0 || c.JoinedRooms() > 0 {
				p.cups = c
				cupsMu.Unlock()
				p.mu.Lock()
				p.shared = true
				p.mu.Unlock()
				p.runShared()
				return true
			}
		}
		cupsMu.Unlock()
		return false
	}
	p.mu.Lock()
	alreadyShared := p.shared
	p.mu.Unlock()
	if alreadyShared {
		return true
	}
	sharedCups := lookupCups(p.rooms)
	if sharedCups == nil && p.rooms != "" {
		// The registration may still carry an empty key while the session is
		// creating its rooms; match on the live room list instead.
		cupsMu.Lock()
		for _, c := range cupsByURL {
			if len(c.RoomIDs()) > 0 && roomSetsEqual(roomSet(p.rooms), roomStringSet(c.RoomIDs())) {
				sharedCups = c
				break
			}
		}
		cupsMu.Unlock()
	}
	if sharedCups == nil && strings.TrimSpace(p.rooms) == "" {
		// No rooms of our own: ride whatever cups the session runs. On an exit
		// the session creates its rooms a few seconds after start, so this is
		// retried on every flush rather than decided once.
		cupsMu.Lock()
		for _, c := range cupsByURL {
			sharedCups = c
			break
		}
		cupsMu.Unlock()
	}
	if sharedCups == nil {
		cupsMu.Lock()
		known := len(cupsByURL)
		cupsMu.Unlock()
		log.Printf("[cbir] adopt: no match (mine %d rooms, registered %d) — queued %d",
			len(roomSet(p.rooms)), known, len(p.pending))
		return false
	}
	p.mu.Lock()
	p.cups = sharedCups
	p.shared = true
	p.mu.Unlock()
	p.runShared()
	return true
}

func (p *cupsPointers) flush() {
	if !p.adopt() {
		return
	}
	p.mu.Lock()
	if !p.ready || p.cups.JoinedRooms() == 0 {
		// No lane yet: cups refuses the send ("striped: no connected lane"),
		// and the ids would be lost. Keep them queued.
		p.mu.Unlock()
		return
	}
	if !p.ready {
		// Not in the rooms yet: keep the ids, the transport is right and the
		// channel is not. Dropping them here is what made every cbirId vanish
		// before the handshake ever started.
		p.mu.Unlock()
		return
	}
	ids := p.pending
	p.pending = nil
	p.mu.Unlock()
	if len(ids) == 0 {
		return
	}
	msg := "OFXPTR1:" + p.tag + ":" + strings.Join(ids, ",")
	send := func([]byte) error { return p.cups.Send([]byte(msg)) }
	if p.shared {
		if raw, ok := any(p.cups).(interface{ SendRaw([]byte) error }); ok {
			send = raw.SendRaw
		}
	}
	if err := send([]byte(msg)); err != nil {
		log.Printf("[cbir] pointer batch (%d) failed: %v", len(ids), err)
		return
	}
	log.Printf("[cbir] pointer batch out: %d ids", len(ids))
}

// runShared installs the filter on a cups transport the session already runs.
func (p *cupsPointers) runShared() {
	tap := func(msg []byte) {
		s := string(msg)
		if !strings.HasPrefix(s, "OFXPTR1:") {
			return
		}
		rest := strings.TrimPrefix(s, "OFXPTR1:")
		colon := strings.Index(rest, ":")
		if colon <= 0 || rest[:colon] == p.tag {
			return
		}
		for _, id := range strings.Split(rest[colon+1:], ",") {
			if id != "" {
				p.tr.DeliverPointer(id)
			}
		}
	}
	if tapper, ok := any(p.cups).(interface{ AddReceiveTap(func([]byte)) }); ok {
		tapper.AddReceiveTap(tap)
		log.Printf("[cbir] pointer tap installed on the cups lane (tag %s)", p.tag)
	}
	go func() {
		for p.cups.JoinedRooms() == 0 {
			time.Sleep(200 * time.Millisecond)
		}
		p.mu.Lock()
		p.ready = true
		p.mu.Unlock()
		log.Printf("[cbir] pointing over the session's cups transport (tag %s), lane up", p.tag)
	}()
	go func() {
		t := time.NewTicker(pointerBatch)
		defer t.Stop()
		for range t.C {
			p.flush()
		}
	}()
}

// run joins the rooms and forwards every foreign id to the transport.
// bindLoop tries to move the channel onto the session's cups transport until
// it succeeds. The exit has to hear the client before it sends anything, so
// this cannot wait for the first flush.
func (p *cupsPointers) bindLoop() {
	for i := 0; ; i++ {
		if p.adopt() {
			return
		}
		if i%4 == 0 {
			cupsMu.Lock()
			n := len(cupsByURL)
			rooms := 0
			for _, c := range cupsByURL {
				rooms += len(c.RoomIDs())
			}
			cupsMu.Unlock()
			log.Printf("[cbir] bind: %d cups registered, %d rooms visible, mine %d", n, rooms, len(roomSet(p.rooms)))
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// bindOrOwn rides the session's cups when there is one and only builds its own
// as a last resort: a standalone instance joins the rooms but the session lane
// already holds the write path, so the two never see each other.
func (p *cupsPointers) bindOrOwn(base transport.TransportConfig) {
	deadline := time.Now().Add(8 * time.Second)
	for {
		if p.adopt() {
			return
		}
		if p.rooms != "" && time.Now().After(deadline) {
			cups := cupsonline.NewCupsonlineTransport(p.rooms, base, true)
			p.mu.Lock()
			p.cups = cups
			p.shared = false
			p.mu.Unlock()
			p.run()
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func (p *cupsPointers) run() {
	cb := func(msg []byte) {
		if os.Getenv("OPENFLUX_CBIR_DEBUG") != "" {
			txt := string(msg)
			if len(txt) > 24 {
				txt = txt[:24]
			}
			log.Printf("[cbir] raw in: %q", txt)
		}
		s := string(msg)
		if len(s) > 40 {
			s = s[:40]
		}
		log.Printf("[cbir] cups in: %q", s)
		s = string(msg)
		if !strings.HasPrefix(s, "OFXPTR1:") {
			return
		}
		rest := strings.TrimPrefix(s, "OFXPTR1:")
		colon := strings.Index(rest, ":")
		if colon <= 0 || rest[:colon] == p.tag {
			return // malformed, or our own echo
		}
		ids := strings.Split(rest[colon+1:], ",")
		for _, id := range ids {
			if id != "" {
				p.tr.DeliverPointer(id)
			}
		}
		if os.Getenv("OPENFLUX_CBIR_DEBUG") != "" {
			log.Printf("[cbir] pointer in: %d ids", len(ids))
		}
	}
	// Start first: the room connection installs its own receive path, so a
	// callback set before it would be replaced.
	if err := p.cups.Start(); err != nil {
		log.Printf("[cbir] cups start FAILED: %v", err)
		return
	}
	p.cups.Receive(cb)
	log.Printf("[cbir] pointer channel started (tag %s, rooms %s)", p.tag, p.roomHint)
	// Wait for the rooms, then let the transport begin announcing.
	go func() {
		t := time.NewTicker(pointerBatch)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				p.flush()
			}
		}
	}()
	deadline := time.Now().Add(2 * time.Minute)
	for !p.cups.IsConnected() && time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
	}
	connected := p.cups.IsConnected()
	p.mu.Lock()
	p.ready = connected
	p.mu.Unlock()
	log.Printf("[cbir] pointer channel connected=%v", connected)
}

// firstRoom names the first room of a packed room list, for the log: it must
// be the same string on both ends or the two sides talk past each other.
func firstRoom(list string) string {
	var packed struct {
		Rooms []string `json:"rooms"`
	}
	if err := json.Unmarshal([]byte(list), &packed); err != nil || len(packed.Rooms) == 0 {
		return "(unparsed)"
	}
	return packed.Rooms[0]
}

func roomStringSet(list []string) map[string]bool {
	out := make(map[string]bool, len(list))
	for _, r := range list {
		out[r] = true
	}
	return out
}
