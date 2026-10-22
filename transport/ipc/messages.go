// Package ipc defines a small framed protocol over a Unix domain socket,
// used by the mobile bridge (iOS/Android) to talk to the Go core.
//
// Frame layout on the wire:
//
//	[4 bytes big-endian length N][1 byte type][N-1 bytes payload]
//
// Length includes the type byte but not the four length bytes themselves.
// Payload is JSON for every type that has one.
package ipc

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// MsgType identifies a frame on the wire.
type MsgType byte

const (
	MsgCookiesRequest MsgType = 0x01
	MsgCookiesOffer   MsgType = 0x02
	MsgStatus         MsgType = 0x03
	MsgCommand        MsgType = 0x04
	MsgLog            MsgType = 0x05
)

// MaxFrameBytes caps a single frame.
const MaxFrameBytes = 1 << 20

type CookiesRequestPayload struct {
	Transport string `json:"transport"`
	URL       string `json:"url"`
	// HTML is a script transport's own setup/login page (see
	// transport/script/js/template_html.html), sent instead of URL when
	// the transport raised one; "" for every native transport.
	HTML   string `json:"html,omitempty"`
	Reason string `json:"reason"`
	// Own marks a page that is the script's own - HTML, or a URL on the
	// script's own loopback server (http://127.0.0.1:port) - rather than a
	// real site: the app shows it as it is, gives it window.openfluxSubmit
	// and does not collect cookies from it. The core decides this
	// (script.IsOwnPage); an app must not guess from the URL.
	Own bool `json:"own,omitempty"`
	// Remote marks a check the exit node needs: it must be passed from the
	// exit's address, and the answering offer must set Remote too.
	Remote bool `json:"remote,omitempty"`
	// Proxy, set with Remote, is a local HTTP proxy (host:port) whose
	// traffic leaves from the exit's address; point the browser at it.
	Proxy string `json:"proxy,omitempty"`
}

type CookiesOfferPayload struct {
	Transport string            `json:"transport"`
	Jar       map[string]string `json:"jar"`
	Domain    string            `json:"domain,omitempty"`
	// Remote sends the jar to the exit node's transport instead of the
	// local one (answer to a Remote request).
	Remote bool `json:"remote,omitempty"`
}

type StatusPayload struct {
	Running   bool   `json:"running"`
	Connected bool   `json:"connected"`
	BytesIn   uint64 `json:"bytes_in"`
	BytesOut  uint64 `json:"bytes_out"`
	UptimeMs  int64  `json:"uptime_ms"`
	// Active names the carrier data currently goes through ("" when none
	// reaches the peer). Sessions only.
	Active string `json:"active,omitempty"`
	// ActiveAll names every carrier data is spread over: several when they
	// share the highest priority. Active is the first. Sessions only.
	ActiveAll []string `json:"active_all,omitempty"`
	// Diagnostics for the "connected but no internet" screen: whether the
	// handshake is established, how long ago the peer last answered, and
	// every carrier's state. False/-1/nil when the transport is not a
	// Session.
	Established bool            `json:"established"`
	LastHeardMs int64           `json:"last_heard_ms"` // -1: the peer was never heard
	Carriers    []CarrierStatus `json:"carriers,omitempty"`
}

// CarrierStatus is one carrier's row of the status diagnostics: Connected
// without Heard is the silent data plane - the carrier's socket is up on
// this side while nothing arrives from the peer on it.
type CarrierStatus struct {
	Name        string `json:"name"`
	Connected   bool   `json:"connected"`
	Heard       bool   `json:"heard"`
	LastHeardMs int64  `json:"last_heard_ms"` // -1: never heard on this carrier
}

type CommandPayload struct {
	Action string                 `json:"action"`
	Params map[string]interface{} `json:"params,omitempty"`
}

type LogPayload struct {
	Line string `json:"line"`
}

// WriteFrame writes a length-prefixed, typed frame.
func WriteFrame(w io.Writer, typ MsgType, payload interface{}) error {
	var body []byte
	if payload != nil {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("ipc: marshal: %w", err)
		}
	}
	n := 1 + len(body)
	if n > MaxFrameBytes {
		return fmt.Errorf("ipc: frame too large (%d bytes)", n)
	}
	var hdr [5]byte
	binary.BigEndian.PutUint32(hdr[:4], uint32(n))
	hdr[4] = byte(typ)
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(body) > 0 {
		if _, err := w.Write(body); err != nil {
			return err
		}
	}
	return nil
}

// ReadFrame reads one frame. Returns io.EOF on clean shutdown.
func ReadFrame(r io.Reader) (MsgType, []byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > MaxFrameBytes {
		return 0, nil, fmt.Errorf("ipc: invalid frame length %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, nil, err
	}
	return MsgType(buf[0]), buf[1:], nil
}

// DecodeJSON unmarshals a frame payload into v.
func DecodeJSON(payload []byte, v interface{}) error {
	if len(payload) == 0 {
		return nil
	}
	return json.Unmarshal(payload, v)
}

// ErrClosed is returned when an IPC operation is attempted on a closed link.
var ErrClosed = errors.New("ipc: closed")
