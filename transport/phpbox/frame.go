// Package phpbox is the client side of the phpbox stream-mux exit: a way to
// reach the internet through a plain PHP host (e.g. free shared hosting)
// that cannot run a binary, hold a long-lived process, or open a listening
// socket. See deploy/phpbox/phpbox.php for the exit.
//
// The host buffers request bodies but streams responses, so one full-duplex
// tunnel is carried over two half-duplex HTTP channels:
//
//	down (GET, long-poll): the session process on the host. It holds the
//	     destination sockets, selects over them, and streams exit->client
//	     frames back in the response body. It also drains the upstream bus
//	     each loop and applies OPEN/DATA/CLOSE.
//	up   (POST, short):    the client's OPEN/DATA/CLOSE frames, appended to a
//	     per-session bus on the host for the down process to pick up.
//
// A session id is minted per down-connection; when the host caps the
// down-poll (~140s) the streams end and the app reconnects, which MTProto
// and short HTTPS tolerate. v0 carries frames in the clear; wrap the carrier
// with the core's encryption before real use.
package phpbox

import "encoding/binary"

// Frame types on the wire.
const (
	FrameOpen    byte = 1 // payload = "host:port", optionally "\x00fc": the client understands FrameAck
	FrameData    byte = 2 // payload = stream bytes
	FrameClose   byte = 3 // payload = empty
	FrameOpenOK  byte = 4 // payload = empty, or "fc": the exit will honour acks on this stream
	FrameOpenErr byte = 5 // payload = reason
	// FrameAck tells the sender how much of its DATA the receiver has consumed, so it never
	// has more than a window in flight. Payload = the receiver's running total, uint32
	// big-endian (cumulative, so a lost ack is repaired by the next one).
	FrameAck byte = 6
)

// headerLen is the fixed frame header: type(1) + streamID(4) + length(4).
const headerLen = 9

// Frame is one mux message for a single logical stream.
type Frame struct {
	Type     byte
	StreamID uint32
	Payload  []byte
}

// Encode appends the wire form of f to dst and returns it.
func Encode(dst []byte, f Frame) []byte {
	var h [headerLen]byte
	h[0] = f.Type
	binary.BigEndian.PutUint32(h[1:], f.StreamID)
	binary.BigEndian.PutUint32(h[5:], uint32(len(f.Payload)))
	dst = append(dst, h[:]...)
	return append(dst, f.Payload...)
}

// TakeFrame pulls one whole frame off the front of buf. It returns the frame,
// the remaining bytes, and true; or ok=false when buf holds less than a full
// frame yet (the caller keeps buf and reads more).
func TakeFrame(buf []byte) (Frame, []byte, bool) {
	if len(buf) < headerLen {
		return Frame{}, buf, false
	}
	n := binary.BigEndian.Uint32(buf[5:])
	if uint32(len(buf)-headerLen) < n {
		return Frame{}, buf, false
	}
	f := Frame{
		Type:     buf[0],
		StreamID: binary.BigEndian.Uint32(buf[1:]),
		Payload:  append([]byte(nil), buf[headerLen:headerLen+n]...),
	}
	return f, buf[headerLen+n:], true
}
