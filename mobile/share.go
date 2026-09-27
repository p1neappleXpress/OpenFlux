package mobile

import (
	"encoding/json"
	"errors"
	"net"
	"sort"

	"openflux/share"
)

// ShareQRPNG renders link as a size x size QR code PNG for the app to show.
func ShareQRPNG(link string, size int) ([]byte, error) {
	return share.PNG(link, size)
}

// ParseShareLink decodes a scanned or opened openflux:// link and returns
// its configuration as JSON (share.Config) for the app to turn into a
// profile.
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
	return share.Encode(c)
}

// exitShareClassic describes a classic single-transport exit to clients.
func exitShareClassic(transportType, documentURL, secret, codec string) *share.Config {
	c := &share.Config{
		Secret:     secret,
		Context:    classicContext(documentURL),
		Transports: []share.Transport{{Type: transportType, URL: documentURL}},
	}
	if codec == "legacy" {
		c.Codec = codec
	}
	return c
}

// exitShareSession describes a Session exit to clients: the same
// transports and context; direct keeps the listen address, whose host is
// replaced in ExitShareLink. MAX is left out (per-account token).
func exitShareSession(specsJSON, secret string) *share.Config {
	specs, context, err := parseSessionSpecs(specsJSON)
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

// classicContext is the classic mode's encryption context, as the core
// derives it for --transport with --url: the document URL, or "http://#"
// when there is none (oneme, direct).
func classicContext(documentURL string) string {
	if documentURL == "" {
		return placeholderURL
	}
	return documentURL
}
