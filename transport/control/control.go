// Package control defines the wire format shared by NegotiatedTransport's
// peers: a small, fixed-size envelope plus kind-specific tails.
//
// Everything that travels over NegotiatedTransport is one of three kinds:
//
//	KindHello   — capability/role handshake, no payload
//	KindIPv4    — exactly one complete IPv4 packet
//	KindControl — an opaque control message (cookies, future extensions)
//
// The envelope is 78 bytes and is authenticated by the outer AES-GCM layer
// (EncryptedTransport). Nothing in this package performs cryptography; it
// only encodes and decodes bytes.
package control

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

// Kind identifies what the envelope carries.
type Kind byte

const (
	KindHello   Kind = 1
	KindIPv4    Kind = 2
	KindControl Kind = 3
)

// EnvelopeSize is the fixed size of the outer header, shared by all kinds.
const EnvelopeSize = 78

// Magic is the first four bytes of every envelope. The trailing byte is the
// envelope version.
var Magic = [4]byte{'O', 'F', 'N', 1}

// Role identifies the sender. Roles must be opposite on the two peers.
type Role byte

const (
	RoleClient Role = 0
	RoleExit   Role = 1
)

// Envelope is the fixed outer header shared by every packet.
//
// Layout (bytes):
//
//	0..3    Magic ("OFN" + version 1)
//	4       Kind
//	5       Role
//	6..37   sender challenge (32 bytes)
//	38..69  recipient challenge echo (32 bytes, zero only on first hello)
//	70..77  kind-specific tail (8 bytes)
type Envelope struct {
	Kind  Kind
	Role  Role
	Local [32]byte // our challenge
	Peer  [32]byte // peer's challenge, echoed back

	Hello   *HelloTail
	Data    *DataTail
	Control *ControlTail
}

// HelloTail is the 8-byte tail of a KindHello envelope.
type HelloTail struct {
	Capabilities  Capabilities
	MaxPacketSize uint16
	Ready         byte
	Reserved      byte
}

// DataTail is the 8-byte tail of a KindIPv4 envelope.
type DataTail struct {
	Sequence uint64
}

// ControlTail is the 8-byte tail of a KindControl envelope. The actual
// payload follows the envelope.
type ControlTail struct {
	Subtype    Subtype
	Flags      byte
	PayloadLen uint16
}

// Encode serializes the envelope into a 78-byte slice.
func (e *Envelope) Encode() ([]byte, error) {
	if e.Kind != KindHello && e.Kind != KindIPv4 && e.Kind != KindControl {
		return nil, fmt.Errorf("control: unknown kind 0x%02x", e.Kind)
	}
	if e.Role != RoleClient && e.Role != RoleExit {
		return nil, fmt.Errorf("control: unknown role 0x%02x", e.Role)
	}
	out := make([]byte, EnvelopeSize)
	copy(out[0:4], Magic[:])
	out[4] = byte(e.Kind)
	out[5] = byte(e.Role)
	copy(out[6:38], e.Local[:])
	copy(out[38:70], e.Peer[:])

	switch e.Kind {
	case KindHello:
		if e.Hello == nil {
			return nil, errors.New("control: hello tail missing")
		}
		binary.BigEndian.PutUint32(out[70:74], uint32(e.Hello.Capabilities))
		binary.BigEndian.PutUint16(out[74:76], e.Hello.MaxPacketSize)
		out[76] = e.Hello.Ready
		out[77] = e.Hello.Reserved
	case KindIPv4:
		if e.Data == nil {
			return nil, errors.New("control: data tail missing")
		}
		binary.BigEndian.PutUint64(out[70:78], e.Data.Sequence)
	case KindControl:
		if e.Control == nil {
			return nil, errors.New("control: control tail missing")
		}
		out[70] = byte(e.Control.Subtype)
		out[71] = e.Control.Flags
		binary.BigEndian.PutUint16(out[72:74], e.Control.PayloadLen)
		// 74..77 reserved zero
	}
	return out, nil
}

// Decode parses a complete packet (envelope + payload) into an Envelope.
// The returned Envelope's tail pointers are set; the trailing payload, if
// any, is NOT copied — callers that need it must slice the original slice
// using PayloadOffset.
func Decode(p []byte) (*Envelope, error) {
	if len(p) < EnvelopeSize {
		return nil, fmt.Errorf("control: packet shorter than envelope (%d bytes)", len(p))
	}
	if !bytes.Equal(p[0:4], Magic[:]) {
		return nil, errors.New("control: bad magic")
	}
	kind := Kind(p[4])
	role := Role(p[5])
	if role != RoleClient && role != RoleExit {
		return nil, fmt.Errorf("control: unknown role 0x%02x", role)
	}
	env := &Envelope{Kind: kind, Role: role}
	copy(env.Local[:], p[6:38])
	copy(env.Peer[:], p[38:70])

	switch kind {
	case KindHello:
		env.Hello = &HelloTail{
			Capabilities:  Capabilities(binary.BigEndian.Uint32(p[70:74])),
			MaxPacketSize: binary.BigEndian.Uint16(p[74:76]),
			Ready:         p[76],
			Reserved:      p[77],
		}
	case KindIPv4:
		env.Data = &DataTail{Sequence: binary.BigEndian.Uint64(p[70:78])}
	case KindControl:
		env.Control = &ControlTail{
			Subtype:    Subtype(p[70]),
			Flags:      p[71],
			PayloadLen: binary.BigEndian.Uint16(p[72:74]),
		}
		if p[74]|p[75]|p[76]|p[77] != 0 {
			return nil, errors.New("control: reserved bytes non-zero")
		}
		if int(env.Control.PayloadLen) != len(p)-EnvelopeSize {
			return nil, fmt.Errorf("control: payload length %d, have %d",
				env.Control.PayloadLen, len(p)-EnvelopeSize)
		}
	default:
		return nil, fmt.Errorf("control: unknown kind 0x%02x", kind)
	}
	return env, nil
}

// PayloadOffset returns the offset at which a kind's payload starts (always
// EnvelopeSize for payload-bearing kinds, or EnvelopeSize when there is none).
func PayloadOffset() int { return EnvelopeSize }
