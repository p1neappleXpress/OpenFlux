package ipc

import (
	"path/filepath"
	"testing"
	"time"
)

type recordingHandler struct {
	commands chan *CommandPayload
	cookies  chan *CookiesOfferPayload
}

func (r *recordingHandler) OnCommand(p *CommandPayload) {
	select {
	case r.commands <- p:
	default:
	}
}

func (r *recordingHandler) OnCookies(p *CookiesOfferPayload) {
	select {
	case r.cookies <- p:
	default:
	}
}

func (r *recordingHandler) OnConnect()    {}
func (r *recordingHandler) OnDisconnect() {}

func TestServerClientRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.sock")

	h := &recordingHandler{
		commands: make(chan *CommandPayload, 1),
		cookies:  make(chan *CookiesOfferPayload, 1),
	}
	srv := NewServer(path, h)
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	cli, err := Dial(path)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	if err := cli.SendCommand(&CommandPayload{Action: "start"}); err != nil {
		t.Fatal(err)
	}
	select {
	case cmd := <-h.commands:
		if cmd.Action != "start" {
			t.Fatalf("cmd = %+v", cmd)
		}
	case <-time.After(time.Second):
		t.Fatal("command not delivered")
	}

	if err := cli.SendCookiesOffer(&CookiesOfferPayload{
		Transport: "yandex",
		Jar:       map[string]string{"a": "1"},
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-h.cookies:
		if c.Jar["a"] != "1" {
			t.Fatalf("cookies = %+v", c)
		}
	case <-time.After(time.Second):
		t.Fatal("cookies not delivered")
	}
}

func TestServerToClient(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.sock")
	srv := NewServer(path, nil)
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	cli, err := Dial(path)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	got := make(chan *CookiesRequestPayload, 1)
	cli.SetHandler(func(typ MsgType, payload []byte) {
		if typ != MsgCookiesRequest {
			return
		}
		var p CookiesRequestPayload
		if err := DecodeJSON(payload, &p); err != nil {
			return
		}
		select {
		case got <- &p:
		default:
		}
	})

	// give the client a moment to install the handler before the server sends
	time.Sleep(50 * time.Millisecond)

	if err := srv.SendCookiesRequest(&CookiesRequestPayload{
		Transport: "yandex",
		URL:       "https://disk.yandex.ru/i/XXX",
		Reason:    "smartcaptcha",
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-got:
		if p.Transport != "yandex" || p.Reason != "smartcaptcha" {
			t.Fatalf("payload = %+v", p)
		}
	case <-time.After(time.Second):
		t.Fatal("cookies request not delivered")
	}
}

func TestServerRejectsNoClient(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.sock")
	srv := NewServer(path, nil)
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	if err := srv.SendStatus(&StatusPayload{Running: true}); err == nil {
		t.Fatal("expected error when no client connected")
	}
}
