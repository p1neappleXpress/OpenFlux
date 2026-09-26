package control

import "fmt"

// Subtype identifies a control message inside a KindControl envelope.
type Subtype byte

const (
	// Cookie exchange.
	//
	//	Request  client -> exit   "please refresh your cookies and send them back"
	//	Response exit   -> client "here are the cookies you asked for"
	//	Offer    exit   -> client "here are fresh cookies, unprompted"
	SubtypeCookiesRequest  Subtype = 0x01
	SubtypeCookiesResponse Subtype = 0x02
	SubtypeCookiesOffer    Subtype = 0x03

	// AuthRequired exit -> client: a transport on the exit is stuck on a
	// captcha or login wall that must be passed from the exit's address.
	// The client answers with SubtypeCookiesOffer naming that transport.
	SubtypeAuthRequired Subtype = 0x04

	// Transport lifecycle. The client drives these; the exit answers with
	// SubtypeTransportStatus. Multiple transports can be active at once:
	// each one is a full Transport with its own NegotiatedTransport.
	//
	//	Start  client -> exit   "bring up transport with config in payload"
	//	Stop   client -> exit   "tear down transport with name in payload"
	//	Status exit   -> client "state of one transport"
	//	List   client -> exit   "send me the current transport list"
	//
	// The payload for Start/Stop is a JSON TransportConfig. For Status it is
	// a JSON TransportStatus. For List it is a JSON TransportStatusList.
	SubtypeTransportStart  Subtype = 0x10
	SubtypeTransportStop   Subtype = 0x11
	SubtypeTransportStatus Subtype = 0x12
	SubtypeTransportList   Subtype = 0x13

	// Per-carrier keepalive. A ping is answered with a pong on the carrier
	// it arrived on, so each side can tell a carrier that is merely attached
	// to its document from one that actually reaches the peer. Peers that
	// predate these ignore them, which the Session detects and tolerates.
	SubtypeLinkPing Subtype = 0x20
	SubtypeLinkPong Subtype = 0x21
)

// ControlPacket is a decoded control message: subtype, flags, payload.
type ControlPacket struct {
	Subtype Subtype
	Flags   byte
	Payload []byte
}

// DecodeWithPayload decodes a full control packet (envelope + payload) in one
// call. It validates the envelope, the reserved bytes, and the payload length.
func DecodeWithPayload(p []byte) (*ControlPacket, *Envelope, error) {
	env, err := Decode(p)
	if err != nil {
		return nil, nil, err
	}
	if env.Kind != KindControl || env.Control == nil {
		return nil, env, fmt.Errorf("control: envelope is not a control packet")
	}
	if env.Control.Flags != 0 {
		return nil, env, fmt.Errorf("control: unknown flags 0x%02x", env.Control.Flags)
	}
	if env.Control.Subtype == 0 {
		return nil, env, fmt.Errorf("control: empty subtype")
	}
	cp := &ControlPacket{
		Subtype: env.Control.Subtype,
		Flags:   env.Control.Flags,
	}
	if env.Control.PayloadLen > 0 {
		cp.Payload = append([]byte(nil), p[EnvelopeSize:]...)
	}
	return cp, env, nil
}
