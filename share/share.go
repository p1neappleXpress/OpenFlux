// Package share turns an OpenFlux client configuration into an openflux://
// link and a QR code, so a client (a phone, another phone running as an
// exit, a desktop) can be set up by scanning instead of copying keys and
// document URLs by hand.
//
// The link carries the encryption secret: whoever sees it can join the
// exit. Treat it like the key file.
package share

import (
	"bytes"
	"compress/flate"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	qrcode "github.com/skip2/go-qrcode"

	"github.com/p1neappleXpress/OpenFlux/utils"
)

// Prefix starts every link; the path segment is the format version. Encode
// writes PrefixV2, Decode reads both: a v1 link keeps working forever, and a
// v2 link tells an old client "update OpenFlux" instead of decoding to noise.
const Prefix = "openflux://v1/"
const PrefixV2 = "openflux://v2/"

// maxPayload bounds the decompressed JSON, so a crafted link cannot make
// the decoder allocate without limit.
const maxPayload = 16 << 10

// Why a link cannot be read or made (Error.Code, Result.Code). The codes
// are the contract with the apps: they word each one for their users, the
// core only decides which it is.
const (
	CodeNotLink            = "not_link"
	CodeUnsupportedVersion = "unsupported_version"
	CodeCaseChanged        = "case_changed"
	CodeDamaged            = "damaged" // not base64url or not DEFLATE: mangled or cut on the way
	CodeTooLarge           = "too_large"
	CodeBadPayload         = "bad_payload" // decompresses, but is not a configuration
	CodeBadConfig          = "bad_config"  // Make: the configuration JSON does not parse
	CodeNoTransports       = "no_transports"
	CodeNeedsSession       = "several_need_session"
	CodeSessionSecret      = "session_secret" // Param: the minimum length
	CodeShortSecret        = "short_secret"   // Param: the minimum length
	CodeUnknownCodec       = "unknown_codec"  // Param: the codec
	CodeNotShareable       = "not_shareable"  // Param: the transport type (MAX)
	CodeUnknownTransport   = "unknown_transport"
	CodeDirectNoDial       = "direct_no_dial"
	CodeDirectNeedsSession = "direct_needs_session"
	CodeUnknownMode        = "unknown_mode"     // Param: the mode
	CodeStreamTransport    = "stream_transport" // Param: the transport type
	CodeStreamOneTransport = "stream_one_transport"
	CodeStreamPlainOnly    = "stream_plain_only" // a session or a secret does not go with stream mode
)

// ModeStream is the phpbox stream mode: the client speaks the stream mux to an
// exit on plain PHP hosting (deploy/phpbox) over one carrier, instead of IP
// packets to a VDS. A link without a mode is the classic tunnel, as it always was.
const ModeStream = "stream"

// streamTypes are the carriers a stream-mode exit speaks: deploy/phpbox has a
// carrier for cups.online and one for Mail.ru documents, and a link names what
// the exit on the other end actually runs.
var streamTypes = map[string]bool{"cupsonline": true, "mailru": true}

// Error is a link that cannot be used: Code says which problem it is (for
// the apps), Param the value it is about, Error() the English detail the
// CLI prints.
type Error struct {
	Code  string
	Param string
	text  string
	cause error
}

func (e *Error) Error() string {
	if e.cause != nil {
		return e.text + ": " + e.cause.Error()
	}
	return e.text
}

func (e *Error) Unwrap() error { return e.cause }

func linkError(code, param, text string, cause error) error {
	return &Error{Code: code, Param: param, text: text, cause: cause}
}

// Transport is one transport the client should run.
type Transport struct {
	Type     string `json:"type"`
	Name     string `json:"name,omitempty"` // defaults to Type
	URL      string `json:"url,omitempty"`
	Priority int    `json:"priority,omitempty"`
	Dial     string `json:"dial,omitempty"` // direct: the exit's host:port
	// Params are what a JS transport takes besides its url: a room key, a
	// token, a channel name. Values are strings so the field never needs a
	// version of its own.
	Params map[string]string `json:"params,omitempty"`
	// Enabled is nil (on) or explicitly off, so a transport can stay in the
	// link while it is not used.
	Enabled *bool `json:"enabled,omitempty"`
	// Role says what the transport carries: "data" (the tunnel's traffic),
	// "control" (pointers and acks only) or "" (both).
	Role string `json:"role,omitempty"`
}

// Config is what a client needs to connect to one exit.
type Config struct {
	// Name is a suggested profile name.
	Name string `json:"name,omitempty"`
	// Negotiate selects an authenticated session (--negotiate); several
	// transports always need one.
	Negotiate bool `json:"negotiate,omitempty"`
	// Codec is "batched" (also when empty) or "legacy".
	Codec string `json:"codec,omitempty"`
	// Secret is the shared encryption secret; required for a session.
	Secret string `json:"secret,omitempty"`
	// Context is the encryption context, the exit's --url; both peers must
	// use the same one.
	Context string `json:"context,omitempty"`
	// Mode is "" (the classic tunnel) or ModeStream. Readers that predate it ignore the
	// field and would run the classic tunnel against a PHP exit, which cannot work, so
	// only links made for stream mode carry it.
	Mode string `json:"mode,omitempty"`
	// Node is the exit's host:port, for links that name it instead of leaving
	// the choice to the app.
	Node string `json:"node,omitempty"`
	// Settings are the knobs that used to be flags (handshake timeout, batch
	// size, dial timeout); the core reads the ones it knows and ignores the rest.
	Settings map[string]string `json:"settings,omitempty"`
	// V is the body version Encode wrote: 0 for a v1 link, 2 for this one.
	V          int         `json:"v,omitempty"`
	Transports []Transport `json:"transports"`
}

// knownTypes are the transports a link can carry. MAX (oneme) is left out:
// the exit's MAX token belongs to the exit's account, and a client needs its
// own.
var knownTypes = map[string]bool{
	"yandex": true, "vyandex": true, "boards": true, "mailru": true,
	"cupsonline": true, "avito": true, "direct": true, "telemost": true,
	// yandex-cbir: the app ships it as a JS transport; the core names it so a
	// link can carry it (the carrier itself is the scripted one).
	"yandex-cbir": true,
}

// jsTypes are the JS transports the app knows, named by
// OPENFLUX_JS_TRANSPORTS ("cbir,vkteams"). The core does not carry the JS
// catalogue, so the app tells it which types a link may name.
func jsTypes() map[string]bool {
	out := map[string]bool{}
	for _, t := range strings.Split(os.Getenv("OPENFLUX_JS_TRANSPORTS"), ",") {
		if t = strings.TrimSpace(t); t != "" {
			out[t] = true
		}
	}
	return out
}

// shareableType reports whether a link may carry this type: one the core
// knows, or a JS transport the app named.
func shareableType(typ string) bool {
	return knownTypes[typ] || jsTypes()[typ]
}

// Shareable reports whether a link can carry a carrier of this type that
// rides a service (a document, a board, a conference): direct, the exit's own
// port, is not one, and MAX is not shareable at all.
func Shareable(typ string) bool {
	if !shareableType(typ) {
		return false
	}
	return typ != "direct"
}

// Validate reports whether c describes something a client can connect with.
func (c *Config) Validate() error {
	if len(c.Transports) == 0 {
		return linkError(CodeNoTransports, "", "share: no transports", nil)
	}
	if c.Mode != "" {
		if err := c.validateStream(); err != nil {
			return err
		}
	}
	if len(c.Transports) > 1 && !c.Negotiate {
		return linkError(CodeNeedsSession, "", "share: several transports need a negotiated session", nil)
	}
	// Characters as Kotlin and Java count them (UTF-16 units), so a link
	// is valid or not the same way on every client.
	if c.Negotiate && utils.SecretChars(c.Secret) < utils.MinSecretChars {
		return linkError(CodeSessionSecret, strconv.Itoa(utils.MinSecretChars),
			fmt.Sprintf("share: a negotiated session needs a secret of at least %d characters", utils.MinSecretChars), nil)
	}
	if c.Secret != "" && utils.SecretChars(c.Secret) < utils.MinSecretChars {
		return linkError(CodeShortSecret, strconv.Itoa(utils.MinSecretChars),
			fmt.Sprintf("share: the secret must be at least %d characters", utils.MinSecretChars), nil)
	}
	if c.Codec != "" && c.Codec != "batched" && c.Codec != "legacy" {
		return linkError(CodeUnknownCodec, c.Codec, fmt.Sprintf("share: unknown codec %q", c.Codec), nil)
	}
	for _, t := range c.Transports {
		if t.Role != "" && t.Role != "data" && t.Role != "control" && t.Role != "both" {
			return linkError(CodeBadPayload, t.Type, fmt.Sprintf("share: unknown transport role %q (data, control or both)", t.Role), nil)
		}
		if !shareableType(t.Type) {
			if t.Type == "oneme" {
				return linkError(CodeNotShareable, t.Type, "share: MAX (oneme) cannot be shared: its token belongs to one account", nil)
			}
			return linkError(CodeUnknownTransport, t.Type, fmt.Sprintf("share: unknown transport type %q", t.Type), nil)
		}
		if t.Type == "direct" {
			if t.Dial == "" {
				return linkError(CodeDirectNoDial, "", "share: direct needs the exit's address", nil)
			}
			if !c.Negotiate {
				return linkError(CodeDirectNeedsSession, "", "share: direct only works in a negotiated session", nil)
			}
		}
	}
	return nil
}

// validateStream checks what stream mode needs: one carrier it can ride, no session.
func (c *Config) validateStream() error {
	if c.Mode != ModeStream {
		return linkError(CodeUnknownMode, c.Mode, fmt.Sprintf("share: unknown mode %q", c.Mode), nil)
	}
	if len(c.Transports) != 1 {
		return linkError(CodeStreamOneTransport, "", "share: stream mode rides exactly one transport", nil)
	}
	if t := c.Transports[0].Type; !streamTypes[t] {
		return linkError(CodeStreamTransport, t, fmt.Sprintf("share: stream mode cannot ride %q (cups.online, mail.ru)", t), nil)
	}
	if c.Negotiate || c.Secret != "" {
		return linkError(CodeStreamPlainOnly, "", "share: stream mode has no session or encryption secret", nil)
	}
	return nil
}

// Encode validates c and returns its openflux:// link.
func Encode(c Config) (string, error) {
	if err := c.Validate(); err != nil {
		return "", err
	}
	c.V = 2
	raw, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	w, err := flate.NewWriter(&buf, flate.BestCompression)
	if err != nil {
		return "", err
	}
	if _, err := w.Write(raw); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	return PrefixV2 + base64.RawURLEncoding.EncodeToString(buf.Bytes()), nil
}

// Decode parses and validates an openflux:// link.
//
// It is lenient about how the link travelled, the same way on every
// client: whitespace and line breaks inside it (a link copied out of a
// terminal or a chat wraps), base64 padding ("=", which some encoders add)
// and the standard base64 alphabet ("+/" for "-_") are all accepted. The
// link Encode writes has none of them.
func Decode(link string) (Config, error) {
	link = strings.TrimSpace(link)
	prefix := Prefix
	if strings.HasPrefix(link, PrefixV2) {
		prefix = PrefixV2
	}
	if !strings.HasPrefix(link, prefix) {
		if strings.HasPrefix(strings.ToLower(link), "openflux://") && !strings.HasPrefix(link, "openflux://") {
			return Config{}, linkError(CodeCaseChanged, "", "share: the link's letters changed case on the way (openflux:// links are case-sensitive); copy it again", nil)
		}
		if strings.HasPrefix(link, "openflux://") {
			return Config{}, linkError(CodeUnsupportedVersion, "", "share: unsupported link version; update OpenFlux", nil)
		}
		return Config{}, linkError(CodeNotLink, "", "share: not an openflux:// link", nil)
	}
	body := normalizeBody(strings.TrimPrefix(link, prefix))
	packed, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return Config{}, linkError(CodeDamaged, "", fmt.Sprintf("share: bad link encoding (%d characters after the prefix; truncated or mangled?)", len(body)), err)
	}
	raw, err := io.ReadAll(io.LimitReader(flate.NewReader(bytes.NewReader(packed)), maxPayload+1))
	if err != nil {
		return Config{}, linkError(CodeDamaged, "", "share: bad link payload (not raw DEFLATE; zlib/gzip-wrapped or truncated?)", err)
	}
	if len(raw) > maxPayload {
		return Config{}, linkError(CodeTooLarge, "", "share: link payload too large", nil)
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return Config{}, linkError(CodeBadPayload, "", "share: bad link payload", err)
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// normalizeBody undoes what copying and other encoders do to the base64url
// part of a link.
func normalizeBody(b string) string {
	b = strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\r', '\n', '\u00a0', '\u200b':
			return -1
		case '+':
			return '-'
		case '/':
			return '_'
		}
		return r
	}, b)
	return strings.TrimRight(b, "=")
}

func qr(link string) (*qrcode.QRCode, error) {
	// Medium correction: survives a slightly glared phone screen while
	// keeping the code small enough to scan off another phone.
	return qrcode.New(link, qrcode.Medium)
}

// Bitmap returns the QR code of link as rows of dark (true) modules,
// including the quiet zone.
func Bitmap(link string) ([][]bool, error) {
	q, err := qr(link)
	if err != nil {
		return nil, err
	}
	return q.Bitmap(), nil
}

// PNG renders the QR code of link as a size x size PNG image.
func PNG(link string, size int) ([]byte, error) {
	q, err := qr(link)
	if err != nil {
		return nil, err
	}
	return q.PNG(size)
}

// Terminal renders the QR code of link with half-block characters, for
// printing in a terminal or a service log.
func Terminal(link string) (string, error) {
	q, err := qr(link)
	if err != nil {
		return "", err
	}
	return q.ToSmallString(false), nil
}
