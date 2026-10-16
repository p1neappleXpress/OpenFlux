package cupsonline

import (
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport"
)

// A striped cups transport runs a transport.StripedTransport over its rooms,
// one lane per room: every room is a WebSocket of its own with its own send
// clock (SendInterval), so N rooms carry N times what one does. The stripe
// numbers the pieces, restores their order on the far side and resends what
// a dropped socket lost, so the tunnel sees one ordered link instead of
// frames that overtake each other between rooms.

// stripePieceBytes is sized so a few whole pieces fill a cups message
// (MaxMessageData, now 12288) with room to spare, less the piece's length
// prefix and the stripe's header and flag. Pieces this size let ~6 of them
// ride one message; a short one (the tail of a frame, an ack) leaves room
// for others. Pieces as big as a whole message left the average message half
// empty - the next piece never fitted beside a tail - and the server takes a
// room's messages at a rate, not its bytes.
const stripePieceBytes = 8192/4 - 2 - 10

// stripeOptions: a cups room loses nothing while its socket is up, so no
// spare-room copies; it answers a busy message in up to a second or so, so a
// lane counts as dead only after seconds of silence.
var stripeOptions = transport.StripeOptions{
	PieceBytes: stripePieceBytes,
	DeadAfter:  3 * time.Second,
	NoDups:     true,
	InitRTO:    2 * time.Second,
	MinRTO:     1500 * time.Millisecond,
	MaxRTO:     8 * time.Second,
	NoPacing:   true,
	// CoDel must sit above the room relay RTT (0.4-1 s) or it drops
	// pieces the relay is still delivering.
	CodelTarget:   1500 * time.Millisecond,
	CodelInterval: 6 * time.Second,
	AckInterval:   100 * time.Millisecond,
	HoleDeadline:  10 * time.Second,
	LaneWindow:    true,
}

// startStripe makes every room a lane and starts the stripe. The rooms' own
// goroutines are started by Start right after, already pulling.
func (t *CupsonlineTransport) startStripe() error {
	lanes := make([]transport.Transport, len(t.wss))
	for i, ws := range t.wss {
		l := &cupsLane{ws: ws}
		ws.onData = l.deliver
		lanes[i] = l
	}
	st := transport.NewStripedTransport(lanes)
	st.SetOptions(stripeOptions)
	st.Receive(t.CallReceive)
	if err := st.Start(); err != nil {
		return err
	}
	t.stripe.Store(st)
	return nil
}

// cupsLane is one room as a stripe lane. The room's connection is run by the
// transport; the lane only hands the stripe its traffic and its clock.
type cupsLane struct {
	ws *cupsWS
	cb func([]byte) // set by the stripe before the room starts
}

func (l *cupsLane) Start() error { return nil }
func (l *cupsLane) Stop() error  { return nil }

// Send carries the stripe's out-of-band frames; its data comes by pull.
func (l *cupsLane) Send(b []byte) error { return l.ws.Send(b) }

func (l *cupsLane) Receive(cb func([]byte)) { l.cb = cb }

// deliver hands the stripe a copy: the room reuses its receive buffer as soon
// as onData returns, and the stripe keeps pieces until their turn comes.
func (l *cupsLane) deliver(b []byte) {
	if l.cb != nil {
		l.cb(append([]byte(nil), b...))
	}
}

func (l *cupsLane) IsConnected() bool { return l.ws.connected.Load() }

func (l *cupsLane) Stats() transport.TransportStats {
	st := l.ws.stats
	return transport.TransportStats{
		BytesSent:     st.bytesSent.Load(),
		BytesReceived: st.bytesRecv.Load(),
		PacketsSent:   st.packetsSent.Load(),
		PacketsRecv:   st.packetsRecv.Load(),
		Reconnects:    st.reconnects.Load(),
		Connected:     l.ws.connected.Load(),
	}
}

// SetFrameSource implements transport.FramePuller.
func (l *cupsLane) SetFrameSource(src func(max int) [][]byte) { l.ws.pull.Store(&src) }

// Wake implements transport.LaneWaker.
func (l *cupsLane) Wake() {
	select {
	case l.ws.wake <- struct{}{}:
	default:
	}
}
