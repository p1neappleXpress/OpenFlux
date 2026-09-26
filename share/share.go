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
	"errors"
	"fmt"
	"io"
	"strings"

	qrcode "github.com/skip2/go-qrcode"
)

// Prefix starts every link; the path segment is the format version.
const Prefix = "openflux://v1/"

// maxPayload bounds the decompressed JSON, so a crafted link cannot make
// the decoder allocate without limit.
const maxPayload = 16 << 10

// Transport is one transport the client should run.
type Transport struct {
	Type     string `json:"type"`
	Name     string `json:"name,omitempty"` // defaults to Type
	URL      string `json:"url,omitempty"`
	Priority int    `json:"priority,omitempty"`
	Dial     string `json:"dial,omitempty"` // direct: the exit's host:port
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
	Context    string      `json:"context,omitempty"`
	Transports []Transport `json:"transports"`
}

// knownTypes are the transports a link can carry. MAX (oneme) is left out:
// the exit's MAX token belongs to the exit's account, and a client needs its
// own.
var knownTypes = map[string]bool{
	"yandex": true, "vyandex": true, "boards": true, "mailru": true,
	"cupsonline": true, "direct": true,
}

// Validate reports whether c describes something a client can connect with.
func (c *Config) Validate() error {
	if len(c.Transports) == 0 {
		return errors.New("share: no transports")
	}
	if len(c.Transports) > 1 && !c.Negotiate {
		return errors.New("share: several transports need a negotiated session")
	}
	if c.Negotiate && len(c.Secret) < 16 {
		return errors.New("share: a negotiated session needs a secret of at least 16 characters")
	}
	if c.Secret != "" && len(c.Secret) < 16 {
		return errors.New("share: the secret must be at least 16 characters")
	}
	if c.Codec != "" && c.Codec != "batched" && c.Codec != "legacy" {
		return fmt.Errorf("share: unknown codec %q", c.Codec)
	}
	for _, t := range c.Transports {
		if !knownTypes[t.Type] {
			return fmt.Errorf("share: unknown transport type %q", t.Type)
		}
		if t.Type == "direct" {
			if t.Dial == "" {
				return errors.New("share: direct needs the exit's address")
			}
			if !c.Negotiate {
				return errors.New("share: direct only works in a negotiated session")
			}
		}
	}
	return nil
}

// Encode validates c and returns its openflux:// link.
func Encode(c Config) (string, error) {
	if err := c.Validate(); err != nil {
		return "", err
	}
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
	return Prefix + base64.RawURLEncoding.EncodeToString(buf.Bytes()), nil
}

// Decode parses and validates an openflux:// link.
func Decode(link string) (Config, error) {
	link = strings.TrimSpace(link)
	if !strings.HasPrefix(link, Prefix) {
		if strings.HasPrefix(link, "openflux://") {
			return Config{}, errors.New("share: unsupported link version; update OpenFlux")
		}
		return Config{}, errors.New("share: not an openflux:// link")
	}
	packed, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(link, Prefix))
	if err != nil {
		return Config{}, fmt.Errorf("share: bad link encoding: %w", err)
	}
	raw, err := io.ReadAll(io.LimitReader(flate.NewReader(bytes.NewReader(packed)), maxPayload+1))
	if err != nil {
		return Config{}, fmt.Errorf("share: bad link payload: %w", err)
	}
	if len(raw) > maxPayload {
		return Config{}, errors.New("share: link payload too large")
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return Config{}, fmt.Errorf("share: bad link payload: %w", err)
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
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
