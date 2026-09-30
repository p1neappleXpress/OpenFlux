package provision

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/p1neappleXpress/OpenFlux/share"
	"github.com/p1neappleXpress/OpenFlux/transport"
)

// ChannelTransport is one of a channel's carriers besides direct, which
// every channel has as the backup.
type ChannelTransport struct {
	// Type is vyandex (a Yandex document), mailru (a Mail.ru public
	// document) or cupsonline (cups.online rooms).
	Type string `json:"type"`
	// URL is the document link, or cups.online's packed room list.
	URL string `json:"url"`
}

// transportPriority is what node-install.sh's write_node_conf gives each
// carrier, and so what the channel's link must say too.
var transportPriority = map[string]int{"vyandex": 100, "mailru": 90, "cupsonline": 70}

const directPriority = 50

var (
	volgaDocURL  = regexp.MustCompile(`^https://(docs|disk)\.yandex\.(ru|com|by|kz|uz)/edit/d/[A-Za-z0-9_-]{16,200}$`)
	mailruDocURL = regexp.MustCompile(`^https://cloud\.mail\.ru/public/[A-Za-z0-9_-]{2,64}/[A-Za-z0-9_-]{2,128}$`)
	cupsRoomList = regexp.MustCompile(`^[A-Za-z0-9_-]{8,}$`)
)

// CheckTransports cleans the links (no query, fragment or trailing slash),
// checks each carrier the way node-install.sh does, and returns them in
// priority order. Each type may appear once; none at all leaves direct only.
func CheckTransports(ts []ChannelTransport) ([]ChannelTransport, error) {
	out := make([]ChannelTransport, 0, len(ts))
	seen := map[string]bool{}
	for _, t := range ts {
		t.Type = strings.TrimSpace(t.Type)
		t.URL = strings.TrimSpace(t.URL)
		if _, ok := transportPriority[t.Type]; !ok {
			return nil, fmt.Errorf("транспорт %q нельзя выбрать для ноды", t.Type)
		}
		if seen[t.Type] {
			return nil, fmt.Errorf("транспорт %s выбран дважды", t.Type)
		}
		seen[t.Type] = true
		switch t.Type {
		case "vyandex":
			t.URL = cleanLink(t.URL)
			if !volgaDocURL.MatchString(t.URL) {
				return nil, errors.New("нужна ссылка на документ Яндекса вида https://docs.yandex.ru/edit/d/…")
			}
		case "mailru":
			t.URL = cleanLink(t.URL)
			if !mailruDocURL.MatchString(t.URL) {
				return nil, errors.New("нужна публичная ссылка Mail.ru вида https://cloud.mail.ru/public/…/…")
			}
		case "cupsonline":
			if !cupsRoomList.MatchString(t.URL) || len(t.URL) > 4096 {
				return nil, errors.New("неверный список комнат cups.online")
			}
		}
		out = append(out, t)
	}
	sort.SliceStable(out, func(i, j int) bool { return transportPriority[out[i].Type] > transportPriority[out[j].Type] })
	return out, nil
}

func cleanLink(u string) string {
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	return strings.TrimRight(u, "/")
}

// TransportURL returns the URL of the carrier of that type, "" if the
// channel has none.
func TransportURL(ts []ChannelTransport, typ string) string {
	for _, t := range ts {
		if t.Type == typ {
			return t.URL
		}
	}
	return ""
}

// SessionContext is the encryption context both sides of the channel
// derive, by the core's one rule (transport.KDFContexts): the
// highest-priority document, cups.online aside (its rooms are no
// document). node-install.sh's session_context picks the same.
func SessionContext(ts []ChannelTransport) string {
	sources := make([]transport.ContextSource, 0, len(ts))
	for _, t := range ts {
		sources = append(sources, transport.ContextSource{Type: t.Type, URL: t.URL, Priority: transportPriority[t.Type]})
	}
	context, _ := transport.KDFContexts("", "", sources)
	return context
}

// ShareLink is the openflux:// link of a new channel: its carriers in
// priority order and direct to host:port as the backup. It carries the
// channel key.
func ShareLink(name, key, host string, port int, ts []ChannelTransport) (string, error) {
	if host == "" || port <= 0 || port > 65535 {
		return "", errors.New("нет адреса или порта ноды")
	}
	ts, err := CheckTransports(ts)
	if err != nil {
		return "", err
	}
	c := share.Config{Name: name, Negotiate: true, Secret: key, Context: SessionContext(ts)}
	for _, t := range ts {
		c.Transports = append(c.Transports, share.Transport{Type: t.Type, URL: t.URL, Priority: transportPriority[t.Type]})
	}
	c.Transports = append(c.Transports, share.Transport{
		Type: "direct", Dial: net.JoinHostPort(host, strconv.Itoa(port)), Priority: directPriority,
	})
	return share.MakeLink(c)
}

// transportLines is the carriers' part of node-install.sh's config.
func transportLines(ts []ChannelTransport) string {
	var b strings.Builder
	for _, t := range ts {
		b.WriteString(t.Type + "=" + t.URL + "\n")
	}
	return b.String()
}
