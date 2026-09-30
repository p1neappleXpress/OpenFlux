package phphost

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/p1neappleXpress/OpenFlux/share"
	"github.com/p1neappleXpress/OpenFlux/transport/cupsonline"
)

// Params is every argument of Call, as JSON; each method reads what it needs.
type Params struct {
	FTP     FTP    `json:"ftp"`
	Token   string `json:"token"`   // the node's token (deploy makes or keeps one)
	URL     string `json:"url"`     // the site: https://example.42web.io
	Carrier string `json:"carrier"` // "cupsonline" | "mailru"
	Target  string `json:"target"`  // the room URL, or the document link the node joins
	Name    string `json:"name"`    // profile name for the link
	Chain   bool   `json:"chain"`   // start the self-renewing node
	// WaitSec is how long start waits for the node to report running (default 25).
	WaitSec int `json:"waitSec"`
}

// NewRoom makes one cups.online room and returns its uuid (or a room URL); tests replace it.
var NewRoom = cupsonline.CreateRoom

var roomUUID = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// RoomURL is the address of a cups.online room, the form the PHP exit and the clients take.
func RoomURL(uuid string) string { return "https://interview.cups.online/live-coding/?room=" + uuid }

// Call is the one entry point every client uses (the desktop wizard's stdin
// protocol, the Android and iOS bridges): method names a step, raw holds its
// Params as JSON, progress (may be nil) hears about uploads. The answer is a
// Result: data, or a code and parameter the app words.
//
//	probe    log in, find the web folder, check it is writable   -> Probe
//	deploy   upload the node                                      -> Installed
//	remove   delete what deploy put there
//	check    does the site answer as a node, can the host run it -> Status
//	start    run the node on Target (waits until it reports)      -> NodeState
//	stop     end the node (the whole chain)
//	node     is the node on Target running                        -> NodeState
//	newRoom  make a cups.online room for the node and clients     -> {"room","url"}
//	link     the openflux:// link clients scan for this node      -> share.Result
func Call(ctx context.Context, method string, raw json.RawMessage, progress func(Progress)) Result {
	var p Params
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return Failed(fail(CodeBadParams, "", "phphost: unreadable parameters", err))
		}
	}
	site := func() *Site { return &Site{URL: p.URL, Token: p.Token, Carrier: p.Carrier} }
	switch method {
	case "probe":
		r, err := ProbeHost(ctx, p.FTP)
		if err != nil {
			if r != nil { // the account was reached; what was learned still helps the app ask the next question
				res := Failed(err)
				res.Data = r
				return res
			}
			return Failed(err)
		}
		return Done(r)
	case "deploy":
		r, err := Deploy(ctx, p.FTP, p.Token, progress)
		if err != nil {
			return Failed(err)
		}
		return Done(r)
	case "remove":
		if err := Remove(ctx, p.FTP); err != nil {
			return Failed(err)
		}
		return Done(nil)
	case "check":
		st, err := site().Check(ctx)
		if err != nil {
			res := Failed(err)
			if st != nil {
				res.Data = st
			}
			return res
		}
		return Done(st)
	case "start":
		wait := time.Duration(p.WaitSec) * time.Second
		if wait <= 0 {
			wait = 25 * time.Second
		}
		ns, err := site().Start(ctx, p.Target, StartOptions{Chain: p.Chain}, wait)
		if err != nil {
			return Failed(err)
		}
		return Done(ns)
	case "stop":
		if err := site().Stop(ctx, p.Target); err != nil {
			return Failed(err)
		}
		return Done(nil)
	case "node":
		ns, err := site().Node(ctx, p.Target)
		if err != nil {
			return Failed(err)
		}
		return Done(ns)
	case "newRoom":
		packed, err := NewRoom(ctx)
		if err != nil {
			return Failed(fail(CodeBadParams, "newRoom", "phphost: could not create a cups.online room", err))
		}
		uuid := FirstRoom(packed)
		if uuid == "" {
			return Failed(fail(CodeBadParams, "newRoom", "phphost: cups.online gave no room", nil))
		}
		return Done(map[string]string{"room": uuid, "url": RoomURL(uuid)})
	case "link":
		t := strings.TrimSpace(p.Target)
		c := share.Config{Name: p.Name, Mode: share.ModeStream, Transports: []share.Transport{{Type: p.Carrier, URL: t}}}
		r := share.Make(c)
		if r.Error != "" {
			res := Result{Error: r.Error, Code: r.Code, Param: r.Param}
			return res
		}
		return Done(r)
	}
	return Failed(fail(CodeBadParams, method, fmt.Sprintf("phphost: unknown method %q", method), nil))
}

// FirstRoom picks one room's UUID out of what CreateRoomList returns (a packed
// list, a bare UUID, or a URL): the PHP exit joins exactly one room.
func FirstRoom(s string) string {
	if u, err := url.Parse(strings.TrimSpace(s)); err == nil && u.Query().Get("room") != "" {
		s = u.Query().Get("room")
	}
	if m := roomUUID.FindString(strings.ToLower(s)); m != "" {
		return m
	}
	// A packed list, as the exit prints it: base64 of a JSON array of uuids.
	if raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(s)); err == nil {
		var ids []string
		if json.Unmarshal(raw, &ids) == nil && len(ids) > 0 {
			return FirstRoom(ids[0])
		}
	}
	return ""
}
