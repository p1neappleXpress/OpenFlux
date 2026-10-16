package mobile

import (
	"strings"
	"testing"

	"github.com/p1neappleXpress/OpenFlux/share"
)

// A cups.online or Avito exit on the phone starts without a room list (it
// creates the rooms); every other carrier, and every client, needs its link.
func TestValidateClassicExitWithoutRoomList(t *testing.T) {
	for _, c := range []struct {
		typ  string
		exit bool
		ok   bool
	}{
		{"cupsonline", true, true},
		{"avito", true, true},
		{"cupsonline", false, false},
		{"avito", false, false},
		{"mailru", true, false},
		{"yandex", true, false},
	} {
		msg := validateClassic(c.typ, "", "", c.exit)
		if (msg == "") != c.ok {
			t.Errorf("%s exit=%v: %q", c.typ, c.exit, msg)
		}
	}
}

type fixedRooms string

func (r fixedRooms) RoomList() string { return string(r) }

// The rooms an exit created go into the link it shares, Avito's as cups'.
func TestExitShareLinkCarriesCreatedRooms(t *testing.T) {
	for _, typ := range []string{"cupsonline", "avito"} {
		exitNode.mu.Lock()
		exitNode.share = exitShareClassic(typ, "", "", "")
		exitNode.rooms = map[string]roomLister{typ: fixedRooms("eyJ2IjoyLCJyb29tcyI6WyJ4Il19")}
		exitNode.mu.Unlock()
		link, err := ExitShareLink("", "phone")
		exitNode.mu.Lock()
		exitNode.share, exitNode.rooms = nil, nil
		exitNode.mu.Unlock()
		if err != nil {
			t.Fatalf("%s: %v", typ, err)
		}
		c, err := share.Decode(link)
		if err != nil {
			t.Fatalf("%s: %v", typ, err)
		}
		if len(c.Transports) != 1 || c.Transports[0].Type != typ || !strings.HasPrefix(c.Transports[0].URL, "eyJ2") {
			t.Fatalf("%s: link carries %+v", typ, c.Transports)
		}
	}
}
