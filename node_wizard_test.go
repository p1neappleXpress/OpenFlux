package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"openflux/provision"
	"openflux/share"
	"openflux/transport/yandex"
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
	for _, m := range []string{"plan", "apply", "setCookies", "remove"} {
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
	if r := wizardCall(t, w, "signedIn", wizardParams{Cookies: "yandexuid=1; Session_id=s"}); r["signedIn"] != true {
		t.Fatalf("signed in: %v", r)
	}
	if r := wizardCall(t, w, "signedIn", wizardParams{Cookies: "yandexuid=1"}); r["signedIn"] != false {
		t.Fatalf("anonymous: %v", r)
	}
}
