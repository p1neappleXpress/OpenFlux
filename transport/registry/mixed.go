package registry

import (
	"fmt"
	"strings"

	"github.com/p1neappleXpress/OpenFlux/transport"
)

// A mixed group stripes carriers of DIFFERENT types: one lane per link, each
// link written "type:link", e.g.
//
//	cupsonline:<room list> avito:<room list> mailru:https://cloud.mail.ru/public/x/y
//
// Every carrier keeps to its own limits (its rooms, its documents) and the
// stripe adds up what they carry, so the sum of the ceilings is the ceiling.
// "type:" with nothing after it is an exit's lane that creates its own rooms;
// RoomList then gives the client the whole line. Both ends need the same line.

// mixedTypes are the carriers a mixed group takes.
var mixedTypes = map[string]bool{
	"cupsonline": true, "avito": true, "mailru": true,
	"yandex": true, "vyandex": true, "boards": true, "telemost": true,
}

type typedLink struct{ typ, link string }

// typedLinks parses a mixed line, or returns nil when url is not one: every
// entry must start with a known "type:" (a "https://..." entry never does).
func typedLinks(url string) []typedLink {
	fields := strings.FieldsFunc(url, func(r rune) bool {
		return r == ',' || r == '|' || r == ' ' || r == '\n' || r == '\t' || r == '\r'
	})
	if len(fields) < 2 {
		return nil
	}
	out := make([]typedLink, 0, len(fields))
	for _, f := range fields {
		i := strings.IndexByte(f, ':')
		if i <= 0 || !mixedTypes[f[:i]] || strings.HasPrefix(f[i+1:], "//") {
			return nil
		}
		out = append(out, typedLink{f[:i], f[i+1:]})
	}
	return out
}

// mixedTransport is the stripe over a mixed group's lanes.
type mixedTransport struct {
	*transport.StripedTransport
	links []typedLink
	rooms []roomed // parallel to links; nil for carriers without rooms
}

// RoomList is the line a client needs: every lane as "type:link", with the
// room carriers' own lists; "" until every room carrier has its rooms.
func (m *mixedTransport) RoomList() string {
	parts := make([]string, 0, len(m.links))
	for i, l := range m.links {
		link := l.link
		if m.rooms[i] != nil {
			if link = m.rooms[i].RoomList(); link == "" {
				return ""
			}
		}
		parts = append(parts, l.typ+":"+link)
	}
	return strings.Join(parts, ",")
}

// OnRoomList calls f with the whole line whenever it changes once complete.
func (m *mixedTransport) OnRoomList(f func(packed string)) {
	for _, r := range m.rooms {
		if r == nil {
			continue
		}
		r.OnRoomList(func(string) {
			if l := m.RoomList(); l != "" {
				f(l)
			}
		})
	}
}

func newMixed(links []typedLink, params map[string]interface{}, o Options) (transport.Transport, error) {
	lanes := make([]transport.Transport, 0, len(links))
	rooms := make([]roomed, len(links))
	anyRooms := false
	hint := 0
	for i, l := range links {
		t, err := New(l.typ, l.link, laneParams(params), o)
		if err != nil {
			return nil, fmt.Errorf("mixed lane %s: %w", l.typ, err)
		}
		if r, ok := t.(roomed); ok {
			rooms[i], anyRooms = r, true
		}
		if h, ok := t.(transport.BatchSizeHinter); ok {
			hint = max(hint, h.PreferredBatchBytes())
			b := transport.NewBatchedTransport(t, h.PreferredBatchBytes())
			b.SetLinger(groupLingerMs)
			t = b
		}
		lanes = append(lanes, t)
	}
	st := transport.NewStripedTransport(lanes)
	// One timeout fits every lane, so it must suit the slowest: a lane that
	// answers in a second must not be resent after half of one.
	st.SetOptions(groupStripe)
	st.SetBatchHint(hint)
	if !anyRooms {
		return st, nil
	}
	return &mixedTransport{StripedTransport: st, links: links, rooms: rooms}, nil
}
