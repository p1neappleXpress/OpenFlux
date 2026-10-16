package registry

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport"
	"github.com/p1neappleXpress/OpenFlux/transport/avito"
	"github.com/p1neappleXpress/OpenFlux/transport/cupsonline"
)

// A group is several carriers of ONE type run as lanes of a single
// transport.StripedTransport: N documents, N rooms, N links. A carrier that
// the server throttles per document or per connection (Mail.ru: ~270 KB/s
// each, measured, a second document doubles it) gets N times the throughput,
// and one lane's drop or throttle stalls only its share of the pieces. The
// stripe numbers the pieces, puts them in order on the far side and resends
// what a dead lane lost, so the lanes need no pairing beyond naming the same
// links on both ends (any order).
//
// The links come from params["urls"] (a list) or from url itself, separated
// by whitespace, commas or '|'. An exit with no url and params["lanes"] = N
// builds N carriers that each create their own rooms; RoomList then gives
// the client the joined links. One link is no group: New builds the carrier
// as before. Both ends must group; a client with a group and an exit without
// one do not understand each other.

// groupStripe tunes the stripe for carriers whose lanes are reliable, ordered
// sockets and that limit messages, not bytes (Mail.ru chat): they lose
// nothing while up, so no spare copies; a lane answers in 1-3 s and a
// throttled one goes quiet for ~10 s, so it counts as dead only after
// seconds of silence. Every ack and every small batch is a chat message and
// a lane gets ~2 of them per second, hence few acks and a long linger.
var groupStripe = transport.StripeOptions{
	PieceBytes:   32 << 10,
	DeadAfter:    12 * time.Second,
	NoDups:       true,
	InitRTO:      3 * time.Second,
	MinRTO:       2 * time.Second,
	MaxRTO:       15 * time.Second,
	HoleDeadline: 20 * time.Second,
	AckInterval:  100 * time.Millisecond,
	LaneWindow:   true,
}

// groupLingerMs is how long a lane's batcher waits to fill a message.
const groupLingerMs = 250

// groupOptions are the stripe settings for lanes of the given type.
func groupOptions(typ string) transport.StripeOptions {
	switch typ {
	case "cupsonline":
		return cupsonline.StripeOptions()
	case "avito":
		return avito.StripeOptions()
	}
	return groupStripe
}

// intParam reads a number a profile may carry as a number or a string.
func intParam(params map[string]interface{}, key string) int {
	switch v := params[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		n, _ := strconv.Atoi(v)
		return n
	}
	return 0
}

// groupURLs returns the links of a group (empty strings for the lanes of an
// exit that creates its own rooms), or nil when the config names one carrier.
func groupURLs(url string, params map[string]interface{}) []string {
	var out []string
	switch v := params["urls"].(type) {
	case []string:
		out = append(out, v...)
	case []interface{}:
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
	}
	if len(out) == 0 && url != "http://#" { // the CLI's "no --url"
		out = strings.FieldsFunc(url, func(r rune) bool {
			return r == ',' || r == '|' || r == ' ' || r == '\n' || r == '\t' || r == '\r'
		})
	}
	if len(out) == 0 {
		if n := intParam(params, "lanes"); n >= 2 && (strings.TrimSpace(url) == "" || url == "http://#") {
			return make([]string, min(n, transport.MaxLanes()))
		}
	}
	var clean []string
	for _, u := range out {
		if u = strings.TrimSpace(u); u != "" {
			clean = append(clean, u)
		}
	}
	if len(clean) < 2 {
		return nil
	}
	return clean
}

// roomed is a carrier that creates rooms of its own and tells the client
// which (cups.online, Avito).
type roomed interface {
	RoomList() string
	OnRoomList(func(packed string))
}

// roomCounter is a roomed carrier that can be told how many rooms to create.
type roomCounter interface{ SetNumRooms(int) }

// groupTransport is the stripe over a group's lanes. When the lanes create
// rooms it is a roomLister too: its list is the lanes' lists joined.
type groupTransport struct {
	*transport.StripedTransport
	rooms []roomed
}

// RoomList is the --url a client needs: every lane's list, joined by commas;
// "" until all lanes have theirs.
func (g *groupTransport) RoomList() string {
	parts := make([]string, 0, len(g.rooms))
	for _, r := range g.rooms {
		l := r.RoomList()
		if l == "" {
			return ""
		}
		parts = append(parts, l)
	}
	return strings.Join(parts, ",")
}

// OnRoomList calls f with the joined list whenever it changes once complete.
func (g *groupTransport) OnRoomList(f func(packed string)) {
	for _, r := range g.rooms {
		r.OnRoomList(func(string) {
			if l := g.RoomList(); l != "" {
				f(l)
			}
		})
	}
}

// newGroup builds one carrier per link and stripes them. Every lane is the
// plain carrier New builds, so a type's own settings apply to each.
func newGroup(typ string, urls []string, params map[string]interface{}, o Options) (transport.Transport, error) {
	switch typ {
	case "direct", "script", "oneme":
		return nil, fmt.Errorf("%s: группа ссылок не поддерживается для этого типа", typ)
	}
	laneRooms := max(intParam(params, "lane_rooms"), 1)
	lanes := make([]transport.Transport, 0, len(urls))
	hint := 0
	var rooms []roomed
	for _, u := range urls {
		l, err := New(typ, u, laneParams(params), o)
		if err != nil {
			return nil, fmt.Errorf("group lane %q: %w", u, err)
		}
		if r, ok := l.(roomed); ok {
			rooms = append(rooms, r)
			if c, ok := l.(roomCounter); ok && u == "" {
				c.SetNumRooms(laneRooms)
			}
		}
		// Pieces and acks are small and many; a carrier limited by messages,
		// not bytes (Mail.ru) must get them packed into big ones, as the
		// Session does above a single carrier. Cups and Avito pack their own.
		if h, ok := l.(transport.BatchSizeHinter); ok {
			hint = max(hint, h.PreferredBatchBytes())
			b := transport.NewBatchedTransport(l, h.PreferredBatchBytes())
			b.SetLinger(groupLingerMs)
			l = b
		}
		lanes = append(lanes, l)
	}
	st := transport.NewStripedTransport(lanes)
	st.SetOptions(groupOptions(typ))
	st.SetBatchHint(hint)
	if len(rooms) != len(lanes) {
		return st, nil // no rooms to hand out (Mail.ru, Yandex, ...)
	}
	return &groupTransport{StripedTransport: st, rooms: rooms}, nil
}

// laneParams is params without the group keys, so a lane is built as a
// single carrier.
func laneParams(params map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(params))
	for k, v := range params {
		if k != "urls" && k != "lanes" && k != "lane_rooms" {
			out[k] = v
		}
	}
	return out
}
