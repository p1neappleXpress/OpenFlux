package phphost

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/p1neappleXpress/OpenFlux/share"
)

func call(t *testing.T, method string, params interface{}, progress func(Progress)) Result {
	t.Helper()
	raw, _ := json.Marshal(params)
	return Call(ctx(t), method, raw, progress)
}

// What the apps call, end to end over JSON: probe, deploy, check.
func TestCallProbeDeployCheck(t *testing.T) {
	srv := newFakeFTP(t, "u", "pw", "htdocs")
	ftp := srv.target()

	r := call(t, "probe", Params{FTP: ftp}, nil)
	if !r.OK {
		t.Fatalf("probe: %+v", r)
	}
	if p := r.Data.(*Probe); p.Dir != "htdocs" || !p.Writable {
		t.Errorf("probe data: %+v", p)
	}

	var seen int
	r = call(t, "deploy", Params{FTP: ftp}, func(Progress) { seen++ })
	if !r.OK || seen == 0 {
		t.Fatalf("deploy: %+v (progress events %d)", r, seen)
	}
	in := r.Data.(*Installed)

	// The JSON the app gets has stable names.
	b, _ := json.Marshal(r)
	var generic struct {
		Data map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(b, &generic); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"dir", "token", "files", "bytes", "security", "token_reused"} {
		if _, ok := generic.Data[k]; !ok {
			t.Errorf("deploy JSON lacks %q: %s", k, b)
		}
	}

	h := fakeHost(t, func(w http.ResponseWriter, q url.Values) {
		if q.Get("k") != in.Token {
			http.Error(w, "no", 404)
			return
		}
		fmt.Fprint(w, `{"phpbox":"0.4","carrier":"cupsonline","php":"8.4","missing":[],"state_dir":true,"parser":true}`)
	})
	if r := call(t, "check", Params{URL: h.URL, Token: in.Token, Carrier: "cupsonline"}, nil); !r.OK {
		t.Errorf("check: %+v", r)
	}
	if r := call(t, "check", Params{URL: h.URL, Token: "wrong", Carrier: "cupsonline"}, nil); r.OK || r.Code != CodeTokenRefused {
		t.Errorf("check with a wrong token: %+v", r)
	}
}

func TestCallErrorsCarryCodes(t *testing.T) {
	srv := newFakeFTP(t, "u", "pw", "htdocs")
	bad := srv.target()
	bad.Password = "nope"
	if r := call(t, "deploy", Params{FTP: bad}, nil); r.OK || r.Code != CodeFTPLogin {
		t.Errorf("wrong password: %+v", r)
	}
	unclear := newFakeFTP(t, "u", "pw", "alpha", "beta")
	r := call(t, "probe", Params{FTP: unclear.target()}, nil)
	if r.OK || r.Code != CodeNoWebRoot || r.Param != "alpha,beta" {
		t.Errorf("unclear web root: %+v", r)
	}
	if p, _ := r.Data.(*Probe); p == nil || len(p.Candidates) != 2 {
		t.Errorf("the candidates should still come back with the failure: %+v", r.Data)
	}
	if r := Call(ctx(t), "probe", json.RawMessage(`{not json`), nil); r.Code != CodeBadParams {
		t.Errorf("garbage params: %+v", r)
	}
	if r := Call(ctx(t), "teleport", nil, nil); r.Code != CodeBadParams || r.Param != "teleport" {
		t.Errorf("unknown method: %+v", r)
	}
}

func TestCallLinkMakesAStreamLinkTheAppsCanRead(t *testing.T) {
	r := call(t, "link", Params{Name: "Free node", Carrier: "mailru", Target: "https://cloud.mail.ru/public/Vuri/d5nuZ5aQp"}, nil)
	if !r.OK {
		t.Fatalf("link: %+v", r)
	}
	made := r.Data.(share.Result)
	back := share.Read(made.Link)
	if back.Config == nil || back.Config.Mode != share.ModeStream || back.Config.Transports[0].Type != "mailru" ||
		back.Config.Transports[0].URL != "https://cloud.mail.ru/public/Vuri/d5nuZ5aQp" || back.Config.Name != "Free node" {
		t.Errorf("read back %+v", back)
	}
	if r := call(t, "link", Params{Carrier: "boards", Target: "https://x"}, nil); r.OK || r.Code != share.CodeStreamTransport {
		t.Errorf("a carrier the PHP exit has no port for: %+v", r)
	}
}

func TestCallNewRoomGivesOneRoom(t *testing.T) {
	old := NewRoom
	t.Cleanup(func() { NewRoom = old })
	NewRoom = func(context.Context) (string, error) { return "0a1b2c3d-1111-2222-3333-444455556666", nil }
	r := call(t, "newRoom", Params{}, nil)
	m, _ := r.Data.(map[string]string)
	if !r.OK || m["room"] != "0a1b2c3d-1111-2222-3333-444455556666" || m["url"] != "https://interview.cups.online/live-coding/?room=0a1b2c3d-1111-2222-3333-444455556666" {
		t.Errorf("newRoom: %+v", r)
	}
}

func TestFirstRoomReadsEveryFormCupsHandsOut(t *testing.T) {
	uuid := "0a1b2c3d-1111-2222-3333-444455556666"
	packed := "WyIwYTFiMmMzZC0xMTExLTIyMjItMzMzMy00NDQ0NTU1NTY2NjYiLCJmZmZmZmZmZi0wMDAwLTAwMDAtMDAwMC0wMDAwMDAwMDAwMDAiXQ" // ["0a1b2c3d-…","ffffffff-…"]
	for _, in := range []string{uuid, RoomURL(uuid), packed, "  " + uuid + "\n"} {
		if got := FirstRoom(in); got != uuid {
			t.Errorf("FirstRoom(%q) = %q", in, got)
		}
	}
	if FirstRoom("not a room") != "" || FirstRoom("") != "" {
		t.Error("garbage gave a room")
	}
}

func TestCallStartWaitsForTheNode(t *testing.T) {
	up := make(chan struct{})
	h := fakeHost(t, func(w http.ResponseWriter, q url.Values) {
		switch q.Get("a") {
		case "run":
			close(up)
		case "status":
			running := false
			select {
			case <-up:
				running = true
			default:
			}
			fmt.Fprintf(w, `{"running":%v,"draining":0,"chain":true,"state":{"gen":1,"phase":"serving"}}`, running)
		}
	})
	r := call(t, "start", Params{URL: h.URL, Token: "t", Carrier: "cupsonline", Target: RoomURL("x"), Chain: true, WaitSec: 10}, nil)
	ns, _ := r.Data.(*NodeState)
	if !r.OK || ns == nil || !ns.Running || !ns.Chain {
		t.Fatalf("start: %+v", r)
	}
	_ = time.Second
}

func TestCallPageIsTheNodesPanelThatOnlyLooks(t *testing.T) {
	for _, c := range []struct{ carrier, file, target string }{
		{"mailru", "/mailruexit.php", "https://cloud.mail.ru/public/Vuri/d5nuZ5aQp"},
		{"cupsonline", "/cupsexit.php", "https://interview.cups.online/live-coding/?room=0a1b2c3d-1111-2222-3333-444455556666"},
	} {
		raw, _ := json.Marshal(map[string]string{"url": "https://site.example.org/", "token": "KEY1", "carrier": c.carrier, "target": c.target})
		res := Call(context.Background(), "page", raw, nil)
		if !res.OK {
			t.Fatalf("%s: %+v", c.carrier, res)
		}
		u, err := url.Parse(res.Data.(map[string]string)["url"])
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		if u.Host != "site.example.org" || u.Path != c.file || q.Get("k") != "KEY1" || q.Get("url") != c.target || q.Get("auto") != "0" {
			t.Errorf("%s: page = %s", c.carrier, u)
		}
	}
}
