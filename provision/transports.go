package provision

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
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
	// Type is vyandex (a Yandex document), yandex (the same document
	// through the older Yandex Docs transport), mailru (a Mail.ru public
	// document), boards (a Yandex board open to guests), cupsonline
	// (cups.online rooms), telemost (a Yandex Telemost conference), avito
	// (Avito Code rooms) or script (a signed .flux JS transport).
	Type string `json:"type"`
	// URL is the document link, a packed room list (cupsonline, avito), or
	// a script's primary parameter.
	URL string `json:"url"`

	// A script carrier: the package's id (its Name in node.conf), the
	// author's ed25519 key (hex) the package is checked against, the .flux
	// itself (base64; only to install or replace it, show leaves it out) and
	// the settings its wizard saved, as script.EncodeSettings writes them.
	Script  string `json:"script,omitempty"`
	Pubkey  string `json:"pubkey,omitempty"`
	Package string `json:"package,omitempty"`
	Params  string `json:"params,omitempty"`
}

// String leaves the package and settings out: a settings value may be a token.
func (t ChannelTransport) String() string {
	if t.Type == "script" {
		return "script:" + t.Script
	}
	return t.Type + " " + t.URL
}

// transportPriority is what node-install.sh's write_node_conf gives each
// carrier, and so what the channel's link must say too. node-install.sh does
// not set up Telemost yet: a channel with it is written by hand, with
// Priority = 85. TODO: confirm Telemost's priority with the core's owner.
var transportPriority = map[string]int{"vyandex": 100, "yandex": 95, "mailru": 90, "telemost": 85, "boards": 80, "avito": 75, "cupsonline": 70, "script": 60}

const directPriority = 50

var (
	volgaDocURL  = regexp.MustCompile(`^https://(docs|disk)\.yandex\.(ru|com|by|kz|uz)/edit/d/[A-Za-z0-9_-]{16,200}$`)
	mailruDocURL = regexp.MustCompile(`^https://cloud\.mail\.ru/public/[A-Za-z0-9_-]{2,64}/[A-Za-z0-9_-]{2,128}$`)
	// yandexDocURL: a document's edit link, or its public disk.yandex.ru/i/ one.
	yandexDocURL = regexp.MustCompile(`^https://(docs|disk)\.yandex\.(ru|com|by|kz|uz)/(edit/d/[A-Za-z0-9_-]{16,200}|i/[A-Za-z0-9_-]{6,64})$`)
	boardsURL    = regexp.MustCompile(`^https://boards\.yandex\.ru/whiteboard/\?hash=[0-9a-f]{32}$`)
	cupsRoomList = regexp.MustCompile(`^[A-Za-z0-9_-]{8,}$`)
	telemostURL  = regexp.MustCompile(`^https://telemost\.yandex\.ru/j/[0-9]{6,32}$`)
	scriptID     = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	hexKey       = regexp.MustCompile(`^[0-9a-f]{64}$`)
	settingsLine = regexp.MustCompile(`^[A-Za-z0-9+/_=-]*$`)
)

// maxPackage bounds a .flux sent to the server inside the installer's config.
const maxPackage = 2 << 20

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
		// Several scripts may run side by side, each package once.
		id := t.Type
		if t.Type == "script" {
			id += ":" + t.Script
		}
		if seen[id] {
			return nil, fmt.Errorf("транспорт %s выбран дважды", t.Type)
		}
		seen[id] = true
		switch t.Type {
		case "vyandex":
			t.URL = cleanLink(t.URL)
			if !volgaDocURL.MatchString(t.URL) {
				return nil, errors.New("нужна ссылка на документ Яндекса вида https://docs.yandex.ru/edit/d/…")
			}
		case "yandex":
			t.URL = cleanLink(t.URL)
			if !yandexDocURL.MatchString(t.URL) {
				return nil, errors.New("нужна ссылка на документ Яндекса вида https://docs.yandex.ru/edit/d/…")
			}
		case "mailru":
			t.URL = cleanLink(t.URL)
			if !mailruDocURL.MatchString(t.URL) {
				return nil, errors.New("нужна публичная ссылка Mail.ru вида https://cloud.mail.ru/public/…/…")
			}
		case "boards":
			t.URL = cleanBoardsLink(t.URL)
			if !boardsURL.MatchString(t.URL) {
				return nil, errors.New("нужна ссылка на доску вида https://boards.yandex.ru/whiteboard/?hash=…")
			}
		case "cupsonline":
			if !cupsRoomList.MatchString(t.URL) || len(t.URL) > 4096 {
				return nil, errors.New("неверный список комнат cups.online")
			}
		case "telemost":
			t.URL = cleanLink(t.URL)
			if !telemostURL.MatchString(t.URL) {
				return nil, errors.New("нужна ссылка на конференцию Телемоста вида https://telemost.yandex.ru/j/…")
			}
		case "avito":
			if !cupsRoomList.MatchString(t.URL) || len(t.URL) > 4096 {
				return nil, errors.New("неверный список комнат Avito")
			}
		case "script":
			if err := checkScript(t); err != nil {
				return nil, err
			}
		}
		out = append(out, t)
	}
	sort.SliceStable(out, func(i, j int) bool { return transportPriority[out[i].Type] > transportPriority[out[j].Type] })
	return out, nil
}

// checkScript: a package id the installer can name a file by, the author's
// key, a parameter and settings that fit one node.conf line, and a package
// that is base64 and not too large.
func checkScript(t ChannelTransport) error {
	if !scriptID.MatchString(t.Script) {
		return fmt.Errorf("JS-транспорт %q: неверный идентификатор пакета", t.Script)
	}
	if !hexKey.MatchString(t.Pubkey) {
		return fmt.Errorf("JS-транспорт %s: нужен ключ автора (64 hex)", t.Script)
	}
	if len(t.URL) > 4096 || strings.ContainsAny(t.URL, "#;\r\n") {
		return fmt.Errorf("JS-транспорт %s: параметр не помещается в node.conf (символы # ; и переводы строк)", t.Script)
	}
	if len(t.Params) > 16384 || !settingsLine.MatchString(t.Params) {
		return fmt.Errorf("JS-транспорт %s: неверные настройки", t.Script)
	}
	if t.Package != "" {
		if len(t.Package) > maxPackage*4/3+4 {
			return fmt.Errorf("JS-транспорт %s: пакет больше %d МБ", t.Script, maxPackage>>20)
		}
		if _, err := base64.StdEncoding.DecodeString(t.Package); err != nil {
			return fmt.Errorf("JS-транспорт %s: пакет не в base64", t.Script)
		}
	}
	return nil
}

// cleanBoardsLink keeps a board link's hash, its only needed query value.
func cleanBoardsLink(u string) string {
	parsed, err := url.Parse(strings.TrimSpace(u))
	if err != nil || parsed.Host != "boards.yandex.ru" {
		return u
	}
	hash := parsed.Query().Get("hash")
	if hash == "" {
		return u
	}
	return "https://boards.yandex.ru/whiteboard/?hash=" + hash
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
	return ShareLinkContext(name, key, host, port, ts, "")
}

// ShareLinkContext is ShareLink for a channel whose encryption context is
// already set: an installed channel whose documents were changed keeps the
// context it was made with (ChannelInfo.Context), whatever its first
// document is now. "" derives it from the documents, as for a new channel.
func ShareLinkContext(name, key, host string, port int, ts []ChannelTransport, context string) (string, error) {
	if host == "" || port <= 0 || port > 65535 {
		return "", errors.New("нет адреса или порта ноды")
	}
	ts, err := CheckTransports(ts)
	if err != nil {
		return "", err
	}
	context = strings.TrimSpace(context)
	if context == "" {
		context = SessionContext(ts)
	}
	c := share.Config{Name: name, Negotiate: true, Secret: key, Context: context}
	for _, t := range ts {
		// A link cannot carry a script package: the app adds its own scripts to the node's profile.
		if !share.Shareable(t.Type) {
			continue
		}
		c.Transports = append(c.Transports, share.Transport{Type: t.Type, URL: t.URL, Priority: transportPriority[t.Type]})
	}
	c.Transports = append(c.Transports, share.Transport{
		Type: "direct", Dial: net.JoinHostPort(host, strconv.Itoa(port)), Priority: directPriority,
	})
	return share.MakeLink(c)
}

// transportLines is the carriers' part of node-install.sh's config. A script
// is one line, script=ID,PUBKEY,PARAMETER,SETTINGS,PACKAGE: the parameter in
// unpadded base64url (it may hold a comma), an empty package keeps the one
// already on the server.
func transportLines(ts []ChannelTransport) string {
	var b strings.Builder
	for _, t := range ts {
		if t.Type == "script" {
			b.WriteString("script=" + t.Script + "," + t.Pubkey + "," + base64.RawURLEncoding.EncodeToString([]byte(t.URL)) + "," + t.Params + "," + t.Package + "\n")
			continue
		}
		b.WriteString(t.Type + "=" + t.URL + "\n")
	}
	return b.String()
}

// ChannelCookies turns the app's Cookie header for the channel's Yandex
// document into what Channel.Cookies takes (see CookieStore), keyed by the
// document as the node's config names it.
func ChannelCookies(ts []ChannelTransport, header string) (string, error) {
	ts, err := CheckTransports(ts)
	if err != nil {
		return "", err
	}
	doc := TransportURL(ts, "vyandex")
	if doc == "" {
		return "", errors.New("вход в Яндекс нужен только каналу с документом Яндекса")
	}
	cookies, _, err := CookieStore(doc, header)
	return cookies, err
}
