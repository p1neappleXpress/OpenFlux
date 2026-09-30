package mobile

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"

	"github.com/p1neappleXpress/OpenFlux/share"
	"github.com/p1neappleXpress/OpenFlux/transport"
)

// ShareQRPNG renders link as a size x size QR code PNG for the app to show.
func ShareQRPNG(link string, size int) ([]byte, error) {
	return share.PNG(link, size)
}

// ReadShareLink reads an openflux:// link with the core's parser and
// returns share.Result as JSON, the same answer the CLI's --parse-link and
// the iOS library give: {"config":...,"context":...} or
// {"error":...,"code":...,"param":...}. The app words the code itself.
func ReadShareLink(link string) string {
	return share.Read(link).JSON()
}

// MakeShareLink builds the link for a share.Config JSON the way every
// client exports one (share.Make) and returns share.Result as JSON:
// {"link":...,"config":...,"context":...} or the error.
func MakeShareLink(configJSON string) string {
	return share.MakeJSON(configJSON).JSON()
}

// ParseShareLink decodes a scanned or opened openflux:// link and returns
// its configuration as JSON (share.Config) for the app to turn into a
// profile. ReadShareLink says why a link is rejected.
func ParseShareLink(link string) (string, error) {
	c, err := share.Decode(link)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(c)
	return string(raw), err
}

// ExitShareLink returns the link other clients scan to use this phone as
// their exit, with direct pointing at host (the phone's address on the
// network the clients share with it) and name as the suggested profile
// name. It fails while the exit is not running.
func ExitShareLink(host, name string) (string, error) {
	exitNode.mu.Lock()
	tmpl, rooms := exitNode.share, exitNode.rooms
	exitNode.mu.Unlock()
	if tmpl == nil {
		return "", errors.New("выходная нода не запущена")
	}
	c := *tmpl
	c.Name = name
	c.Transports = nil
	for _, t := range tmpl.Transports {
		switch t.Type {
		case "direct":
			_, port, err := net.SplitHostPort(t.Dial)
			if err != nil || host == "" {
				return "", errors.New("direct: нет адреса, по которому клиенты смогут подключиться")
			}
			t.Dial = net.JoinHostPort(host, port)
		case "cupsonline":
			// Started without a room list, the exit creates its rooms at
			// start; clients need that list to join them.
			if t.URL == "" {
				key := t.Name
				if key == "" {
					key = t.Type
				}
				if r := rooms[key]; r != nil {
					t.URL = r.RoomList()
				}
			}
			if t.URL == "" {
				appendLog("[ANDROID] Cups: комнаты ещё не созданы, в ссылку не попали")
				continue
			}
		}
		c.Transports = append(c.Transports, t)
	}
	return share.MakeLink(c)
}

// exitShareClassic describes a classic single-transport exit to clients.
// The link stays classic (older clients read it too; updated ones upgrade
// to a Session on their own), except for direct, which a link can only
// carry as a Session.
func exitShareClassic(transportType, documentURL, secret, codec string) *share.Config {
	context, _ := transport.KDFContexts("", documentURL, []transport.ContextSource{{Type: transportType, URL: documentURL, Priority: 100}})
	t := share.Transport{Type: transportType, URL: documentURL}
	c := &share.Config{Secret: secret, Context: context}
	if transportType == "direct" {
		t = share.Transport{Type: "direct", Dial: documentURL}
		c.Negotiate = true
	}
	c.Transports = []share.Transport{t}
	if codec == transport.CodecLegacy {
		c.Codec = codec
	}
	return c
}

// exitShareSession describes a Session exit to clients: the same
// transports and context; direct keeps the listen address, whose host is
// replaced in ExitShareLink. MAX is left out (per-account token).
func exitShareSession(specsJSON, secret string) *share.Config {
	specs, context, _, err := parseSessionSpecs(specsJSON)
	if err != nil {
		return nil
	}
	c := &share.Config{Negotiate: true, Secret: secret, Context: context}
	sorted := append([]sessionSpec(nil), specs...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Priority > sorted[j].Priority })
	for _, s := range sorted {
		t := share.Transport{Type: s.Type, URL: s.URL, Priority: s.Priority}
		if s.Name != s.Type {
			t.Name = s.Name
		}
		switch s.Type {
		case "oneme":
			continue
		case "direct":
			listen, _ := s.Params["listen"].(string)
			if listen == "" {
				listen, _ = s.Params["dial"].(string)
			}
			t.Dial = listen
		}
		c.Transports = append(c.Transports, t)
	}
	return c
}

// ShareSessionSpecs turns an openflux:// link into the profile StartSession
// (and StartSessionProxy / StartSessionExit) takes: {"context":...,
// "transports":[{name,type,url,priority,params}]}, with the link's own
// context and carrier names, so an app does not have to interpret the link
// itself (and cannot drift from the other clients doing so). A one-carrier
// link works there too: the Session speaks classic to a classic node.
func ShareSessionSpecs(link string) (string, error) {
	c, err := share.Decode(link)
	if err != nil {
		return "", err
	}
	return sessionSpecsOf(c), nil
}

// sessionSpecsOf is ShareSessionSpecs for a link already read.
func sessionSpecsOf(c share.Config) string {
	specs := make([]sessionSpec, 0, len(c.Transports))
	seen := make(map[string]int)
	for _, t := range c.Transports {
		name := t.Name
		if name == "" {
			seen[t.Type]++
			name = t.Type
			if n := seen[t.Type]; n > 1 {
				name = fmt.Sprintf("%s-%d", t.Type, n)
			}
		}
		spec := sessionSpec{Name: name, Type: t.Type, URL: t.URL, Priority: t.Priority}
		if t.Type == "direct" {
			spec.URL = ""
			spec.Params = map[string]interface{}{"dial": t.Dial}
		}
		specs = append(specs, spec)
	}
	b, _ := json.Marshal(struct {
		Context    string        `json:"context"`
		Transports []sessionSpec `json:"transports"`
	}{share.ContextOf(c), specs})
	return string(b)
}
