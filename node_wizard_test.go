//go:build !exitnode

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/p1neappleXpress/OpenFlux/provision"
	"github.com/p1neappleXpress/OpenFlux/share"
	"github.com/p1neappleXpress/OpenFlux/transport"
	"github.com/p1neappleXpress/OpenFlux/transport/yandex"
)

func wizardCall(t *testing.T, w *nodeWizard, method string, params interface{}) map[string]interface{} {
	t.Helper()
	raw, _ := json.Marshal(params)
	return w.handle(wizardRequest{ID: 1, Method: method, Params: raw})
}

func TestNodeWizardProtocolLines(t *testing.T) {
	in := strings.NewReader("{\"id\":7,\"method\":\"newChannel\"}\n\nnot json\n{\"id\":9,\"method\":\"nope\"}\n")
	var out bytes.Buffer
	if code := runNodeWizard(in, &out); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 responses, got %d: %q", len(lines), out.String())
	}
	var first, bad, unknown map[string]interface{}
	for i, into := range []*map[string]interface{}{&first, &bad, &unknown} {
		if err := json.Unmarshal([]byte(lines[i]), into); err != nil {
			t.Fatal(err)
		}
	}
	if first["id"] != float64(7) || first["ok"] != true || len(first["key"].(string)) != 64 || first["channel"] == "" {
		t.Fatalf("newChannel: %v", first)
	}
	if bad["ok"] != false {
		t.Fatalf("bad line: %v", bad)
	}
	if unknown["id"] != float64(9) || unknown["ok"] != false {
		t.Fatalf("unknown method: %v", unknown)
	}
}

func TestNodeWizardNeedsConnection(t *testing.T) {
	w := newNodeWizard()
	for _, m := range []string{"plan", "apply", "remove"} {
		r := wizardCall(t, w, m, map[string]string{"channel": "c1"})
		if r["ok"] != false || r["error"] != "нет подключения к серверу" {
			t.Fatalf("%s: %v", m, r)
		}
	}
}

func TestNodeWizardHostKeyIsReportedNotTrusted(t *testing.T) {
	w := newNodeWizard()
	var got provision.Target
	w.dial = func(_ context.Context, target provision.Target) (*provision.Conn, error) {
		got = target
		if target.HostKey == "" {
			return nil, &provision.HostKeyError{Fingerprint: "SHA256:abc"}
		}
		return nil, &provision.HostKeyError{Fingerprint: "SHA256:new", Mismatch: true}
	}
	r := wizardCall(t, w, "connect", wizardParams{Host: " vds.example ", Port: 2222, User: " root ", Password: "pw"})
	if r["ok"] != false || r["hostKey"] != "SHA256:abc" || r["trust"] != true || r["mismatch"] != false {
		t.Fatalf("new server: %v", r)
	}
	if got.Host != "vds.example" || got.User != "root" || got.Port != 2222 || got.Password != "pw" {
		t.Fatalf("target: %+v", got)
	}
	r = wizardCall(t, w, "connect", wizardParams{Host: "vds.example", User: "root", Password: "pw", HostKey: "SHA256:abc"})
	if r["ok"] != false || r["mismatch"] != true || r["trust"] != false {
		t.Fatalf("changed key: %v", r)
	}
	if strings.Contains(fmt.Sprint(r), "pw") {
		t.Fatalf("password leaked into the reply: %v", r)
	}
}

func TestNodeWizardCheckDocument(t *testing.T) {
	w := newNodeWizard()
	w.checkDoc = func(u string) (yandex.VolgaDocument, error) {
		switch u {
		case "edit":
			return yandex.VolgaDocument{Editable: true}, nil
		case "view":
			return yandex.VolgaDocument{}, nil
		case "pow":
			return yandex.VolgaDocument{}, fmt.Errorf("captcha solve: %w", errors.New("captcha POST unexpected status 400"))
		default:
			return yandex.VolgaDocument{}, fmt.Errorf("wrapped: %w", yandex.ErrCaptchaRequired)
		}
	}
	if r := wizardCall(t, w, "checkDocument", wizardParams{DocumentURL: "edit"}); r["ok"] != true || r["editable"] != true {
		t.Fatalf("editable: %v", r)
	}
	if r := wizardCall(t, w, "checkDocument", wizardParams{DocumentURL: "view"}); r["ok"] != false || r["captcha"] == true {
		t.Fatalf("view only: %v", r)
	}
	if r := wizardCall(t, w, "checkDocument", wizardParams{DocumentURL: "captcha"}); r["ok"] != false || r["captcha"] != true {
		t.Fatalf("captcha: %v", r)
	}
	// A PoW captcha Yandex rejected says nothing about the document either.
	if r := wizardCall(t, w, "checkDocument", wizardParams{DocumentURL: "pow"}); r["ok"] != false || r["captcha"] != true {
		t.Fatalf("rejected PoW captcha: %v", r)
	}
}

func TestNodeWizardShareLinkAndSignIn(t *testing.T) {
	w := newNodeWizard()
	key := strings.Repeat("ab", 32)
	doc := "https://docs.yandex.ru/edit/d/AbCdEfGhIjKlMnOpQrStUv"
	r := wizardCall(t, w, "shareLink", wizardParams{Name: "Нода", DocumentURL: doc, Key: key, Host: "203.0.113.5", ChannelPort: 8445})
	if r["ok"] != true {
		t.Fatalf("shareLink: %v", r)
	}
	c, err := share.Decode(r["link"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Negotiate || c.Secret != key || c.Context != doc || len(c.Transports) != 2 ||
		c.Transports[0].Type != "vyandex" || c.Transports[1].Dial != "203.0.113.5:8445" {
		t.Fatalf("link config: %+v", c)
	}
	if r := wizardCall(t, w, "shareLink", wizardParams{DocumentURL: doc, Key: key}); r["ok"] != false {
		t.Fatalf("no host: %v", r)
	}
}

func TestNodeWizardShareLinkWithChosenTransports(t *testing.T) {
	w := newNodeWizard()
	key := strings.Repeat("ab", 32)
	mailru := "https://cloud.mail.ru/public/DEmN/ETbZW2MPY"
	r := wizardCall(t, w, "shareLink", wizardParams{Name: "Нода", Key: key, Host: "203.0.113.5", ChannelPort: 8445,
		Transports: []provision.ChannelTransport{{Type: "cupsonline", URL: "WyJyb29tLTEiXQ"}, {Type: "mailru", URL: mailru}}})
	if r["ok"] != true {
		t.Fatalf("shareLink: %v", r)
	}
	c, err := share.Decode(r["link"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if c.Context != mailru || len(c.Transports) != 3 || c.Transports[0].Type != "mailru" ||
		c.Transports[1].Type != "cupsonline" || c.Transports[2].Dial != "203.0.113.5:8445" {
		t.Fatalf("link config: %+v", c)
	}
	r = wizardCall(t, w, "shareLink", wizardParams{Key: key, Host: "203.0.113.5", ChannelPort: 8445,
		Transports: []provision.ChannelTransport{{Type: "boards", URL: "https://boards.yandex.ru/x"}}})
	if r["ok"] != false {
		t.Fatalf("a transport the wizard does not offer: %v", r)
	}
}

func TestNodeWizardCreateRooms(t *testing.T) {
	w := newNodeWizard()
	w.newRooms = func(context.Context) (string, error) { return "WyJyb29tLTEiXQ", nil }
	if r := wizardCall(t, w, "createRooms", nil); r["ok"] != true || r["rooms"] != "WyJyb29tLTEiXQ" {
		t.Fatalf("createRooms: %v", r)
	}
	w.newRooms = func(context.Context) (string, error) { return "", errors.New("403") }
	if r := wizardCall(t, w, "createRooms", nil); r["ok"] != false || !strings.Contains(r["error"].(string), "cups.online") {
		t.Fatalf("createRooms failure: %v", r)
	}
}

// The node.conf node-install.sh writes and the link the wizard builds must
// agree on everything the two sides derive keys and routes from: the
// encryption context and each carrier's address and priority.
func TestNodeConfMatchesShareLink(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	body, err := os.ReadFile("deploy/node-install.sh")
	if err != nil {
		t.Fatal(err)
	}
	// The script's functions without its command dispatch.
	funcs := string(body)[:strings.Index(string(body), "\ncase \"${1:-}\" in")]
	volga := "https://disk.yandex.ru/edit/d/AbCdEfGhIjKlMnOpQrStUv"
	mailru := "https://cloud.mail.ru/public/DEmN/ETbZW2MPY"
	rooms := "WyJyb29tLTEiLCJyb29tLTIiXQ"
	key := strings.Repeat("cd", 32)
	for _, ts := range [][]provision.ChannelTransport{
		{{Type: "vyandex", URL: volga}},
		{{Type: "vyandex", URL: volga}, {Type: "mailru", URL: mailru}, {Type: "cupsonline", URL: rooms}},
		{{Type: "mailru", URL: mailru}, {Type: "cupsonline", URL: rooms}},
		{{Type: "cupsonline", URL: rooms}},
		nil,
	} {
		vars := "CHANNEL=of-test PORT=20443 URL='" + provision.TransportURL(ts, "vyandex") + "' MAILRU='" +
			provision.TransportURL(ts, "mailru") + "' CUPS='" + provision.TransportURL(ts, "cupsonline") + "'\n"
		cmd := exec.Command(sh)
		cmd.Stdin = strings.NewReader(funcs + "\n" + vars + "write_node_conf\n")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%+v: %v", ts, err)
		}
		path := filepath.Join(t.TempDir(), "node.conf")
		if err := os.WriteFile(path, out, 0o600); err != nil {
			t.Fatal(err)
		}
		conf, err := parseConf(path)
		if err != nil {
			t.Fatalf("%+v: %v\n%s", ts, err, out)
		}
		var specs []transportSpec
		for _, s := range conf.Transports {
			specs = append(specs, transportSpec{Name: s.Name, Type: s.Values["Type"], Priority: confInt(s.Values["Priority"], 50), URL: s.Values["URL"]})
		}
		link, err := provision.ShareLink("n", key, "203.0.113.5", 20443, ts)
		if err != nil {
			t.Fatal(err)
		}
		c, _ := share.Decode(link)
		sources := make([]transport.ContextSource, len(specs))
		for i, s := range specs {
			sources[i] = transport.ContextSource{Type: s.Type, URL: s.URL, Priority: s.Priority}
		}
		if got, _ := transport.KDFContexts("", conf.Interface["URL"], sources); got != c.Context {
			t.Fatalf("%+v: node derives context %q, link says %q\n%s", ts, got, c.Context, out)
		}
		if len(specs) != len(c.Transports) {
			t.Fatalf("%+v: node has %d carriers, link %d\n%s", ts, len(specs), len(c.Transports), out)
		}
		for i, s := range specs {
			lt := c.Transports[i]
			// share.Make drops the priority of a lone carrier: nothing to rank.
			samePriority := s.Priority == lt.Priority || len(c.Transports) == 1 && lt.Priority == 0
			if s.Type != lt.Type || !samePriority || (s.Type != "direct" && s.URL != lt.URL) {
				t.Fatalf("%+v: carrier %d: node %+v, link %+v", ts, i, s, lt)
			}
		}
	}
}
