package main

// --node-wizard: the desktop app's "Своя нода" wizard talks to the core
// over stdin/stdout, one JSON object per line, like the phone calls
// mobile/node.go. The core does the SSH work (package provision), checks the
// channel's Yandex document and builds the channel's openflux:// link; the
// app proves the channel by connecting to it as usual.
//
// Request:  {"id": 1, "method": "connect", "params": {...}}
// Response: {"id": 1, "ok": true, ...} or {"id": 1, "ok": false, "error": "..."}
//
// Secrets (SSH and sudo passwords, private key, channel key, Yandex
// cookies) arrive only on stdin and never go to the log or the command line.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"openflux/provision"
	"openflux/share"
	"openflux/transport/yandex"
)

type wizardRequest struct {
	ID     int64           `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type wizardParams struct {
	Host         string `json:"host"`
	Port         int    `json:"port"`
	User         string `json:"user"`
	Password     string `json:"password"`
	PrivateKey   string `json:"privateKey"`
	Passphrase   string `json:"passphrase"`
	HostKey      string `json:"hostKey"`
	Channel      string `json:"channel"`
	ChannelPort  int    `json:"channelPort"`
	WithCookies  bool   `json:"withCookies"`
	DocumentURL  string `json:"documentUrl"`
	Key          string `json:"key"`
	SudoPassword string `json:"sudoPassword"`
	Cookies      string `json:"cookies"`
	Name         string `json:"name"`
}

// nodeWizard holds the SSH connection between calls.
type nodeWizard struct {
	conn *provision.Conn
	// Tests replace these to run without a VDS or Yandex.
	dial      func(context.Context, provision.Target) (*provision.Conn, error)
	checkDoc  func(string) (yandex.VolgaDocument, error)
	newScript func() provision.Script
}

func newNodeWizard() *nodeWizard {
	return &nodeWizard{
		dial:      provision.Dial,
		checkDoc:  func(u string) (yandex.VolgaDocument, error) { return yandex.CheckVolgaDocument(u, nil) },
		newScript: provision.Pinned,
	}
}

// runNodeWizard serves requests until stdin closes.
func runNodeWizard(in io.Reader, out io.Writer) int {
	w := newNodeWizard()
	defer w.disconnect()
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	enc := json.NewEncoder(out)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var req wizardRequest
		var resp map[string]interface{}
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			resp = wizardFailure(errors.New("неверный запрос"), nil)
		} else {
			resp = w.handle(req)
		}
		resp["id"] = req.ID
		if err := enc.Encode(resp); err != nil {
			return 1
		}
	}
	return 0
}

func wizardOK(fields map[string]interface{}) map[string]interface{} {
	if fields == nil {
		fields = map[string]interface{}{}
	}
	fields["ok"] = true
	return fields
}

func wizardFailure(err error, extra map[string]interface{}) map[string]interface{} {
	fields := map[string]interface{}{"ok": false, "error": err.Error()}
	for k, v := range extra {
		fields[k] = v
	}
	return fields
}

func (w *nodeWizard) handle(req wizardRequest) map[string]interface{} {
	var p wizardParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return wizardFailure(errors.New("неверные параметры"), nil)
		}
	}
	switch req.Method {
	case "connect":
		return w.connect(p)
	case "disconnect":
		w.disconnect()
		return wizardOK(nil)
	case "newChannel":
		id, err := provision.NewChannelID()
		if err != nil {
			return wizardFailure(err, nil)
		}
		key, err := provision.NewKey()
		if err != nil {
			return wizardFailure(err, nil)
		}
		return wizardOK(map[string]interface{}{"channel": id, "key": key})
	case "plan":
		conn, err := w.connected()
		if err != nil {
			return wizardFailure(err, nil)
		}
		plan, err := conn.Plan(p.Channel, p.ChannelPort, p.WithCookies)
		if err != nil {
			return wizardFailure(err, nil)
		}
		return wizardOK(map[string]interface{}{"plan": plan})
	case "apply":
		conn, err := w.connected()
		if err != nil {
			return wizardFailure(err, nil)
		}
		ch := provision.Channel{ID: p.Channel, URL: p.DocumentURL, Key: p.Key, Port: p.ChannelPort}
		if p.Cookies != "" {
			if ch.Cookies, _, err = provision.CookieStore(p.DocumentURL, p.Cookies); err != nil {
				return wizardFailure(err, nil)
			}
		}
		if err := conn.Apply(ch, p.SudoPassword); err != nil {
			return wizardFailure(err, map[string]interface{}{"sudo": errors.Is(err, provision.ErrSudoPassword)})
		}
		return wizardOK(nil)
	case "setCookies":
		conn, err := w.connected()
		if err != nil {
			return wizardFailure(err, nil)
		}
		cookies, _, err := provision.CookieStore(p.DocumentURL, p.Cookies)
		if err != nil {
			return wizardFailure(err, nil)
		}
		if err := conn.SetCookies(p.Channel, cookies, p.SudoPassword); err != nil {
			return wizardFailure(err, map[string]interface{}{"sudo": errors.Is(err, provision.ErrSudoPassword)})
		}
		return wizardOK(nil)
	case "remove":
		conn, err := w.connected()
		if err != nil {
			return wizardFailure(err, nil)
		}
		if err := conn.Remove(p.Channel, p.SudoPassword); err != nil {
			return wizardFailure(err, map[string]interface{}{"sudo": errors.Is(err, provision.ErrSudoPassword)})
		}
		return wizardOK(nil)
	case "signedIn":
		_, signedIn, err := provision.CookieStore("x", p.Cookies)
		return wizardOK(map[string]interface{}{"signedIn": err == nil && signedIn})
	case "checkDocument":
		return w.checkDocument(p.DocumentURL)
	case "shareLink":
		link, err := nodeShareLink(p.Name, p.DocumentURL, p.Key, p.Host, p.ChannelPort)
		if err != nil {
			return wizardFailure(err, nil)
		}
		return wizardOK(map[string]interface{}{"link": link})
	default:
		return wizardFailure(fmt.Errorf("неизвестная команда %q", req.Method), nil)
	}
}

// connect opens SSH, has the VDS download the pinned installer and probes
// it. A new server comes back with "hostKey" and "trust": true so the user
// can compare the fingerprint; a changed key with "mismatch": true.
func (w *nodeWizard) connect(p wizardParams) map[string]interface{} {
	w.disconnect()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	conn, err := w.dial(ctx, provision.Target{
		Host: strings.TrimSpace(p.Host), Port: p.Port, User: strings.TrimSpace(p.User),
		Password: p.Password, PrivateKey: p.PrivateKey, Passphrase: p.Passphrase, HostKey: p.HostKey,
	})
	if err != nil {
		var hk *provision.HostKeyError
		if errors.As(err, &hk) {
			return wizardFailure(err, map[string]interface{}{"hostKey": hk.Fingerprint, "trust": !hk.Mismatch, "mismatch": hk.Mismatch})
		}
		return wizardFailure(err, nil)
	}
	if err := conn.FetchScript(w.newScript()); err != nil {
		conn.Close()
		return wizardFailure(err, nil)
	}
	probe, err := conn.Probe()
	if err != nil {
		conn.Close()
		return wizardFailure(err, nil)
	}
	if !probe.Systemd {
		conn.Close()
		return wizardFailure(errors.New("на сервере нет systemd: мастер поддерживает Debian, Ubuntu и похожие системы"), nil)
	}
	if probe.Sudo == "none" {
		conn.Close()
		return wizardFailure(errors.New("у пользователя нет root и sudo: войдите как root или пользователь с sudo"), nil)
	}
	w.conn = conn
	return wizardOK(map[string]interface{}{"probe": probe})
}

func (w *nodeWizard) disconnect() {
	if w.conn != nil {
		w.conn.Close()
		w.conn = nil
	}
}

func (w *nodeWizard) connected() (*provision.Conn, error) {
	if w.conn == nil {
		return nil, errors.New("нет подключения к серверу")
	}
	return w.conn, nil
}

// checkDocument tells whether the vyandex transport can use the document as
// an anonymous visitor, like the node: {"editable"}. A check Yandex wants a
// person to pass comes back with "captcha": true.
func (w *nodeWizard) checkDocument(documentURL string) map[string]interface{} {
	doc, err := w.checkDoc(documentURL)
	if err != nil {
		captcha := errors.Is(err, yandex.ErrCaptchaRequired) || errors.Is(err, yandex.ErrLoginRequired)
		msg := err
		switch {
		case captcha:
			msg = errors.New("Яндекс просит пройти проверку, повторите через минуту")
		case strings.Contains(err.Error(), "client-config"), strings.Contains(err.Error(), "officeActionData"):
			msg = errors.New("документ не открылся в редакторе Яндекса: проверьте доступ по ссылке")
		}
		return wizardFailure(msg, map[string]interface{}{"captcha": captcha})
	}
	if !doc.Editable {
		return wizardFailure(errors.New("по ссылке документ открывается только на просмотр, нужен доступ на редактирование"), nil)
	}
	return wizardOK(map[string]interface{}{"editable": true})
}

// nodeShareLink is the openflux:// link of a new channel, as the phone's
// wizard builds it: the Yandex document first, direct to host:port as the
// backup. It carries the channel key.
func nodeShareLink(name, documentURL, key, host string, port int) (string, error) {
	if host == "" || port <= 0 || port > 65535 {
		return "", errors.New("нет адреса или порта ноды")
	}
	return share.Encode(share.Config{
		Name:      name,
		Negotiate: true,
		Secret:    key,
		Context:   documentURL,
		Transports: []share.Transport{
			{Type: "vyandex", URL: documentURL, Priority: 100},
			{Type: "direct", Dial: net.JoinHostPort(host, strconv.Itoa(port)), Priority: 50},
		},
	})
}
