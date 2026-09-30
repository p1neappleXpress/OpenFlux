//go:build !exitnode

package main

// --node-wizard: the desktop app's "Своя нода" wizard talks to the core
// over stdin/stdout, one JSON object per line. The core does the SSH work
// (package provision), checks the channel's Yandex document and builds the
// channel's openflux:// link; the app proves the channel by connecting to it
// as usual.
//
// Request:  {"id": 1, "method": "connect", "params": {...}}
// Response: {"id": 1, "ok": true, ...} or {"id": 1, "ok": false, "error": "..."}
//
// Secrets (SSH and sudo passwords, private key, channel key) arrive only
// on stdin and never go to the log or the command line.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/p1neappleXpress/OpenFlux/provision"
	"github.com/p1neappleXpress/OpenFlux/provision/phphost"
	"github.com/p1neappleXpress/OpenFlux/transport/cupsonline"
	"github.com/p1neappleXpress/OpenFlux/transport/yandex"
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
	DocumentURL  string `json:"documentUrl"`
	Key          string `json:"key"`
	SudoPassword string `json:"sudoPassword"`
	Name         string `json:"name"`
	// Transports are the channel's carriers besides direct. Without them,
	// DocumentURL alone means a Yandex document (older apps).
	Transports []provision.ChannelTransport `json:"transports"`
	AutoUpdate bool                         `json:"autoUpdate"`
}

// transports is the channel's carriers from the request.
func (p wizardParams) transports() []provision.ChannelTransport {
	if p.Transports == nil && p.DocumentURL != "" {
		return []provision.ChannelTransport{{Type: "vyandex", URL: p.DocumentURL}}
	}
	return p.Transports
}

// channel is the channel the request describes, its key aside.
func (p wizardParams) channel() provision.Channel {
	return provision.Channel{ID: p.Channel, Transports: p.transports(), Port: p.ChannelPort, AutoUpdate: p.AutoUpdate}
}

// nodeWizard holds the SSH connection between calls.
type nodeWizard struct {
	conn *provision.Conn
	// Tests replace these to run without a VDS or Yandex.
	dial      func(context.Context, provision.Target) (*provision.Conn, error)
	checkDoc  func(string) (yandex.VolgaDocument, error)
	newScript func() provision.Script
	newRooms  func(context.Context) (string, error)
}

func newNodeWizard() *nodeWizard {
	return &nodeWizard{
		dial:      provision.Dial,
		checkDoc:  func(u string) (yandex.VolgaDocument, error) { return yandex.CheckVolgaDocument(u, nil) },
		newScript: provision.Pinned,
		newRooms:  cupsonline.CreateRoomList,
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
		} else if strings.HasPrefix(req.Method, "php.") {
			resp = phpCall(req, enc)
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
		plan, err := conn.Plan(p.channel())
		if err != nil {
			return wizardFailure(err, nil)
		}
		return wizardOK(map[string]interface{}{"plan": plan})
	case "apply":
		conn, err := w.connected()
		if err != nil {
			return wizardFailure(err, nil)
		}
		ch := p.channel()
		ch.Key = p.Key
		if err := conn.Apply(ch, p.SudoPassword); err != nil {
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
	case "checkDocument":
		return w.checkDocument(p.DocumentURL)
	case "createRooms":
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		rooms, err := w.newRooms(ctx)
		if err != nil {
			return wizardFailure(fmt.Errorf("не удалось создать комнаты cups.online: %v", err), nil)
		}
		return wizardOK(map[string]interface{}{"rooms": rooms})
	case "shareLink":
		link, err := provision.ShareLink(p.Name, p.Key, p.Host, p.ChannelPort, p.transports())
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
		// A challenge this computer could not pass, SmartCaptcha or a PoW
		// captcha Yandex rejected ("captcha solve: ..."), says nothing about
		// the document: the node opens it from its own address.
		captcha := errors.Is(err, yandex.ErrCaptchaRequired) || errors.Is(err, yandex.ErrLoginRequired) ||
			strings.HasPrefix(err.Error(), "captcha solve:")
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

// phpCall serves the "php.*" methods of the same protocol: putting the PHP
// exit on a free web host over FTP (package phphost does every step; this only
// carries its answers). Method "php.deploy" is phphost's "deploy", and so on.
// While it runs, upload progress goes out as extra lines
// {"id": N, "progress": {...}} before the final answer, which is
// {"id": N, "ok": true, "data": ...} or {"id": N, "ok": false, "error": "...",
// "code": "...", "param": "..."}: the code and param are what the app words.
func phpCall(req wizardRequest, enc *json.Encoder) map[string]interface{} {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	res := phphost.Call(ctx, strings.TrimPrefix(req.Method, "php."), req.Params, func(p phphost.Progress) {
		_ = enc.Encode(map[string]interface{}{"id": req.ID, "progress": p})
	})
	var resp map[string]interface{}
	_ = json.Unmarshal([]byte(res.JSON()), &resp)
	if resp == nil {
		resp = map[string]interface{}{"ok": false, "error": "internal error"}
	}
	return resp
}
