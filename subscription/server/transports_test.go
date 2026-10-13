package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/p1neappleXpress/OpenFlux/share"
)

const telemostRoom = "https://telemost.yandex.ru/j/88936358186517"

// writeTypedChannel is a channel whose node.conf names each transport's type
// (writeChannel guesses it from the document).
func writeTypedChannel(t *testing.T, dir, name, context string, ts map[string]string, port string) {
	t.Helper()
	base := filepath.Join(dir, name)
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	conf := "Role = exit\nMode = l4\nURL = " + context + "\n"
	for typ, url := range ts {
		conf += "\n[Transport " + typ + "]\nType = " + typ + "\nPriority = 90\nURL = " + url + "\n"
	}
	conf += "\n[Transport direct]\nType = direct\nPriority = 50\nListen = 0.0.0.0:" + port + "\n"
	if err := os.WriteFile(filepath.Join(base, "node.conf"), []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "encryption-key"), []byte(strings.Repeat("ab", 32)+"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
}

// get sends a GET with the app's transports header; "" sends none.
func (e *env) get(path, auth, transports string) (int, reply) {
	e.t.Helper()
	req, _ := http.NewRequest("GET", e.http.URL+path, nil)
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	if transports != "" {
		req.Header.Set(transportsHeader, transports)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out reply
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// links are the document's links by profile id.
func links(t *testing.T, doc reply) map[string]share.Config {
	t.Helper()
	out := map[string]share.Config{}
	for _, p := range doc.list("profiles") {
		r := reply(p.(map[string]any))
		c, err := share.Decode(r.str("link"))
		if err != nil {
			t.Fatalf("profile %s: %v", r.str("id"), err)
		}
		out[r.str("id")] = c
	}
	return out
}

func types(c share.Config) string {
	var ts []string
	for _, t := range c.Transports {
		ts = append(ts, t.Type)
	}
	sort.Strings(ts)
	return strings.Join(ts, ",")
}

// An app gets only the transports it says it runs; one that says nothing gets
// what links carried before Telemost. The context is the channel's either way.
func TestLinkCarriesTheAppsTransports(t *testing.T) {
	e := newEnv(t)
	writeTypedChannel(t, e.nodes, "fork-all", mailruDoc, map[string]string{"mailru": mailruDoc, "telemost": telemostRoom}, "34443")
	token, _ := e.register("ivan", "a")

	_, doc := e.get("/api/v1/subscription", token, "")
	old := links(t, doc)["main"]
	if types(old) != "direct,mailru" {
		t.Fatalf("an app without the header got %s", types(old))
	}
	_, doc = e.get("/api/v1/subscription", token, "mailru, Telemost ,vyandex")
	now := links(t, doc)["main"]
	if types(now) != "direct,mailru,telemost" {
		t.Fatalf("an app that runs Telemost got %s", types(now))
	}
	if old.Context != mailruDoc || now.Context != mailruDoc {
		t.Fatalf("contexts %q and %q: both must be the channel's", old.Context, now.Context)
	}
	// Both sets stay cached side by side.
	if _, doc = e.get("/api/v1/subscription", token, ""); types(links(t, doc)["main"]) != "direct,mailru" {
		t.Fatal("the cache gave one app's link to another")
	}
}

// A location with none of the app's transports is left out of its document,
// not broken, and no node is made for it.
func TestPoolWithoutTheAppsTransportsIsLeftOut(t *testing.T) {
	e := newEnv(t)
	writeTypedChannel(t, e.nodes, "de", telemostRoom, map[string]string{"telemost": telemostRoom}, "41000")
	token, _ := e.register("ivan", "a")

	_, doc := e.get("/api/v1/subscription", token, "")
	if got := links(t, doc); len(got) != 1 || got["main"].Name == "" {
		t.Fatalf("an old app must get the other location only: %v", doc)
	}
	_, doc = e.get("/api/v1/subscription", token, "mailru,telemost")
	if got := links(t, doc); len(got) != 2 || types(got["de"]) != "direct,telemost" {
		t.Fatalf("an app that runs Telemost must get both: %v", doc)
	}
	_, doc = e.get("/api/v1/subscription", token, "cupsonline")
	if doc["profiles"] == nil || len(doc.list("profiles")) != 0 {
		t.Fatalf(`no location fits: "profiles":[] expected, got %v`, doc)
	}
	// The owner's checks see the channel as it is: present, all transports.
	if _, err := e.srv.links.link(e.srv.cfg.Pools[1], "de", nil); err != nil {
		t.Fatalf("the channel is fine for the owner: %v", err)
	}
}

// What a link cannot carry is dropped from the header: MAX, direct, script
// packages (not read yet), junk, and whatever lies past the limits.
func TestTransportsHeaderJunk(t *testing.T) {
	r, _ := http.NewRequest("GET", "/", nil)
	r.Header.Set(transportsHeader, "oneme, direct, script:mtslink@1, ../../etc, ,MAILRU")
	if got := clientTransports(r); got.key() != "mailru" {
		t.Fatalf("got %q", got.key())
	}
	r.Header.Set(transportsHeader, strings.Repeat("x,", 40)+"mailru")
	if got := clientTransports(r); len(got) != 0 {
		t.Fatalf("an entry past 32 was read: %q", got.key())
	}
	r.Header.Set(transportsHeader, strings.Repeat("y", 2000)+",mailru")
	if got := clientTransports(r); len(got) != 0 {
		t.Fatalf("an entry past 1 KB was read: %q", got.key())
	}
	r.Header.Set(transportsHeader, "  ")
	if got := clientTransports(r); got.key() != legacyTransports.key() {
		t.Fatalf("an empty header is no header: %q", got.key())
	}
	r.Header.Del(transportsHeader)
	r.Header.Add(transportsHeader, "mailru")
	r.Header.Add(transportsHeader, "telemost")
	if got := clientTransports(r); got.key() != "mailru,telemost" {
		t.Fatalf("two header lines: %q", got.key())
	}
}

// A key without an account reads the header too.
func TestKeyDocumentHonorsTheAppsTransports(t *testing.T) {
	e := newEnv(t)
	writeTypedChannel(t, e.nodes, "de", boardDoc, map[string]string{"boards": boardDoc, "telemost": telemostRoom}, "41000")
	code, k := e.admin("POST", "/admin/keys", map[string]any{"name": "Подарок", "days": 7, "pools": []string{"de"}})
	if code != 201 {
		t.Fatalf("add key: %d %v", code, k)
	}
	_, doc := e.get("/sub/"+k.str("token"), "", "")
	if got := types(links(t, doc)["de"]); got != "boards,direct" {
		t.Fatalf("no header: %s", got)
	}
	_, doc = e.get("/sub/"+k.str("token"), "", "boards,telemost")
	if got := types(links(t, doc)["de"]); got != "boards,direct,telemost" {
		t.Fatalf("with Telemost: %s", got)
	}
}
