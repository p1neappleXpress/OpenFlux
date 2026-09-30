package main

import (
	"strings"
	"testing"

	"github.com/p1neappleXpress/OpenFlux/provision"
	"github.com/p1neappleXpress/OpenFlux/share"
)

// The link an exit prints must decode to what a client needs: the exit's
// transports, key and context, with direct dialing the shared host.
func TestShareConfigFromExitSpecs(t *testing.T) {
	specs := []transportSpec{
		{Name: "direct", Type: "direct", Priority: 100, Params: map[string]interface{}{"listen": "0.0.0.0:8445", "is_exit": true}},
		{Name: "yandex", Type: "yandex", Priority: 50, URL: "https://disk.yandex.ru/i/abc"},
		{Name: "oneme", Type: "oneme", Priority: 10},
	}
	c, skipped := shareConfig(specs, true, codecBatched, "a shared secret of 32 characters", "https://disk.yandex.ru/i/abc", "203.0.113.7", nil)
	if len(skipped) != 1 || !strings.HasPrefix(skipped[0], "oneme") {
		t.Fatalf("skipped = %v, want the MAX transport only", skipped)
	}
	link, err := share.Encode(c)
	if err != nil {
		t.Fatal(err)
	}
	got, err := share.Decode(link)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Negotiate || got.Context != "https://disk.yandex.ru/i/abc" || len(got.Transports) != 2 ||
		got.Transports[0].Dial != "203.0.113.7:8445" || got.Transports[1].URL != "https://disk.yandex.ru/i/abc" {
		t.Fatalf("decoded %+v", got)
	}

	_, skipped = shareConfig(specs[:1], true, codecBatched, "a shared secret of 32 characters", "", "", nil)
	if len(skipped) != 1 {
		t.Fatal("direct without a host to share must be left out")
	}
}

type fakeRooms struct{ list string }

func (f *fakeRooms) RoomList() string        { return f.list }
func (f *fakeRooms) OnRoomList(func(string)) {}

// A cupsonline exit started without --url creates its rooms at start; the
// link must carry them, or the client has nothing to join.
func TestShareConfigCarriesCreatedCupsRooms(t *testing.T) {
	specs := []transportSpec{{Name: "cupsonline", Type: "cupsonline", Priority: 50}}
	rooms := map[string]roomLister{"cupsonline": &fakeRooms{}}

	_, skipped := shareConfig(specs, true, codecBatched, "a shared secret of 32 characters", "http://#", "", rooms)
	if len(skipped) != 1 || !strings.HasPrefix(skipped[0], "cupsonline") {
		t.Fatalf("skipped = %v, want cupsonline left out before its rooms exist", skipped)
	}

	rooms["cupsonline"].(*fakeRooms).list = "WyJyb29tLTEiXQ"
	c, skipped := shareConfig(specs, true, codecBatched, "a shared secret of 32 characters", "http://#", "", rooms)
	if len(skipped) != 0 || len(c.Transports) != 1 || c.Transports[0].URL != "WyJyb29tLTEiXQ" {
		t.Fatalf("transports %+v, skipped %v", c.Transports, skipped)
	}
}

// --parse-link and --make-link answer with share.Result, byte for byte
// what package mobile and the iOS library answer, and the wizard's link is
// the one share.Make builds for its configuration.
func TestLinkCommandsAnswerLikeShare(t *testing.T) {
	cfg := `{"name":"node","negotiate":true,"secret":"a shared secret of 32 characters",` +
		`"transports":[{"type":"vyandex","url":"https://docs.yandex.ru/edit/d/AbC","priority":100}]}`
	var out strings.Builder
	if code := runMakeLink("-", strings.NewReader(cfg), &out); code != 0 {
		t.Fatalf("--make-link exit %d: %s", code, out.String())
	}
	made := share.MakeJSON(cfg)
	if out.String() != made.JSON()+"\n" {
		t.Fatalf("--make-link %s, share.MakeJSON %s", out.String(), made.JSON())
	}

	wrapped := made.Link[:40] + "\r\n" + made.Link[40:]
	out.Reset()
	if code := runParseLink("-", strings.NewReader(wrapped), &out); code != 0 || out.String() != share.Read(made.Link).JSON()+"\n" {
		t.Fatalf("--parse-link exit %d: %s", code, out.String())
	}
	out.Reset()
	if code := runParseLink("https://example.com", nil, &out); code != 1 || !strings.Contains(out.String(), `"code":"not_link"`) {
		t.Fatalf("--parse-link on garbage: exit %d %s", code, out.String())
	}

	// The wizard's link for a Yandex-document channel is share.NodeConfig's.
	doc := "https://docs.yandex.ru/edit/d/AbCdEfGhIjKlMnOpQrStUv"
	link, err := provision.ShareLink("node", "a shared secret of 32 characters", "203.0.113.7", 9443,
		[]provision.ChannelTransport{{Type: "vyandex", URL: doc}})
	want := share.Make(share.NodeConfig("node", doc, "a shared secret of 32 characters", "203.0.113.7:9443"))
	if err != nil || link != want.Link {
		t.Fatalf("wizard link %q (%v), share.Make %q", link, err, want.Link)
	}
}
