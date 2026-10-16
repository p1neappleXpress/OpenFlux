package registry

import (
	"reflect"
	"testing"
)

func TestGroupURLs(t *testing.T) {
	cases := []struct {
		name   string
		url    string
		params map[string]interface{}
		want   []string
	}{
		{"single", "https://cloud.mail.ru/public/a/b", nil, nil},
		{"comma", "u1,u2", nil, []string{"u1", "u2"}},
		{"mixed separators", "u1 | u2,\nu3", nil, []string{"u1", "u2", "u3"}},
		{"one link with trailing comma", "u1,", nil, nil},
		{"params list", "", map[string]interface{}{"urls": []interface{}{"a", "b"}}, []string{"a", "b"}},
		{"lanes without url", "", map[string]interface{}{"lanes": 3}, []string{"", "", ""}},
		{"lanes from json number", "http://#", map[string]interface{}{"lanes": float64(2)}, []string{"", ""}},
		{"lanes ignored when links are given", "u1,u2", map[string]interface{}{"lanes": 5}, []string{"u1", "u2"}},
		{"lanes 1 is no group", "", map[string]interface{}{"lanes": 1}, nil},
	}
	for _, c := range cases {
		if got := groupURLs(c.url, c.params); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: groupURLs = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestGroupRejectsUnsupportedTypes(t *testing.T) {
	for _, typ := range []string{"direct", "script", "oneme"} {
		if _, err := New(typ, "a,b", nil, Options{}); err == nil {
			t.Errorf("%s: a group should be refused", typ)
		}
	}
}

// A group of cups exits creates its own rooms only at Start; before that the
// joined list is empty, and a plain stripe (Mail.ru) is no roomLister at all.
func TestGroupRoomListWaitsForAllLanes(t *testing.T) {
	g, err := New("cupsonline", "", map[string]interface{}{"lanes": 3}, Options{IsExit: true})
	if err != nil {
		t.Fatal(err)
	}
	rl, ok := g.(interface{ RoomList() string })
	if !ok {
		t.Fatal("a group of cups lanes should hand out a room list")
	}
	if rl.RoomList() != "" {
		t.Fatalf("list before Start = %q, want empty", rl.RoomList())
	}
	m, err := New("mailru", "https://cloud.mail.ru/public/a/b,https://cloud.mail.ru/public/c/d", nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.(interface{ RoomList() string }); ok {
		t.Fatal("a Mail.ru group has no rooms to hand out")
	}
}

func TestTypedLinks(t *testing.T) {
	got := typedLinks("cupsonline:eyJ2 avito:abc,mailru:https://cloud.mail.ru/public/a/b")
	want := []typedLink{{"cupsonline", "eyJ2"}, {"avito", "abc"}, {"mailru", "https://cloud.mail.ru/public/a/b"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("typedLinks = %v, want %v", got, want)
	}
	if got := typedLinks("cupsonline: avito:"); len(got) != 2 || got[0].link != "" {
		t.Fatalf("empty links are lanes that create rooms: %v", got)
	}
	for _, bad := range []string{"https://a/b,https://c/d", "mailru:x", "cupsonline:a foo:b", "u1,u2"} {
		if got := typedLinks(bad); got != nil {
			t.Errorf("typedLinks(%q) = %v, want nil", bad, got)
		}
	}
}

func TestMixedRoomListWaitsForRoomCarriers(t *testing.T) {
	g, err := New("cupsonline", "cupsonline: avito:", nil, Options{IsExit: true})
	if err != nil {
		t.Fatal(err)
	}
	rl, ok := g.(interface{ RoomList() string })
	if !ok {
		t.Fatal("a mixed group of room carriers should hand out a room list")
	}
	if rl.RoomList() != "" {
		t.Fatalf("list before Start = %q, want empty", rl.RoomList())
	}
}
