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
	Reason    string `json:"reason"`
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
