package avito

import (
	"encoding/binary"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport"
)

// A striped Avito transport runs a transport.StripedTransport over its room
// pairs, one lane per pair: the exit's tx room i is the client's rx room i
// and the other way round, so a lane sends into one room and hears the far
// side in the other. A room takes one op at a time and answers it in a few
// hundred milliseconds whatever its size, so an op carries as many pieces as
// fill ChunkBytes (the bench optimum, 256 KB), and N pairs carry N ops at
// once. The stripe numbers the pieces, puts them back in order on the far
// side and resends what a failed op lost.

// stripePieceBytes: an op carries several, so a piece need not fill one; a
// big one keeps the stripe's per-piece bookkeeping small.
const stripePieceBytes = 32 << 10

// stripeOptions: a room loses nothing while its socket is up, so no
// spare-room copies; an op and the ack that comes back on the other room
// take a second or so when the rooms are busy, so a lane counts as dead only
// after seconds of silence.
var stripeOptions = transport.StripeOptions{
	PieceBytes: stripePieceBytes,
	DeadAfter:  5 * time.Second,
	NoDups:     true,
	// An op takes ~0.4-1.5 s to be acked and the ack of the stripe comes back
	// on another room; with the default 500 ms to 2 s timeout nearly every
	// piece was sent twice (16 pairs: 898 resends for 901 pieces).
	InitRTO: 3 * time.Second,
	MinRTO:  2 * time.Second,
	MaxRTO:  10 * time.Second,
}

// lanePollIdle is how long an idle lane sleeps before it asks the stripe
// again without being woken.
const lanePollIdle = 50 * time.Millisecond

// startStripe pairs the rooms into lanes and starts the stripe. The rooms'
// own goroutines are started by Start right after, already pulling.
func (t *AvitoTransport) startStripe() error {
	n := min(len(t.txWss), len(t.rxWss))
	lanes := make([]transport.Transport, n)
	for i := 0; i < n; i++ {
		l := &avitoLane{tx: t.txWss[i], rx: t.rxWss[i]}
		l.rx.onData = l.deliver
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

// pullLoop is sendLoop for a lane's tx room: whenever the room is free (the
// last op acked) it takes pieces from the stripe up to ChunkBytes and sends
// them as one op. What the room cannot take yet waits in the stripe's queue.
func (w *avitoWS) pullLoop(c *avitoConn, src func(max int) [][]byte) error {
	var hdr [4]byte
	idle := time.NewTimer(time.Hour)
	defer idle.Stop()
	var blob []byte
	for {
		blob = blob[:0]
		for len(blob) < w.config.ChunkBytes {
			pieces := src(1)
			if len(pieces) == 0 {
				break
			}
			for _, p := range pieces {
				binary.BigEndian.PutUint32(hdr[:], uint32(len(p)))
				blob = append(blob, hdr[:]...)
				blob = append(blob, p...)
			}
		}
		if len(blob) == 0 {
			idle.Reset(lanePollIdle)
			select {
			case <-w.ctx.Done():
				return errStopped
			case <-c.done:
				return errStopped
			case <-w.wake:
			case <-idle.C:
			}
			idle.Stop()
			continue
		}
		if err := w.sendOp(c, blob); err != nil {
			return err
		}
	}
}

// splitFramed hands each piece of an op's blob to f. The pieces are slices of
// blob, which nobody reuses.
func splitFramed(blob []byte, f func([]byte)) {
	for len(blob) >= 4 {
		n := int(binary.BigEndian.Uint32(blob))
		if n <= 0 || n > len(blob)-4 {
			return // not ours, or cut short: the stripe resends
		}
		f(blob[4 : 4+n])
		blob = blob[4+n:]
	}
}

// avitoLane is one room pair as a stripe lane. The rooms are run by the
// transport; the lane hands the stripe their traffic and the tx room's turns.
type avitoLane struct {
	tx, rx *avitoWS
	cb     func([]byte) // set by the stripe before the rooms start
}

func (l *avitoLane) Start() error { return nil }
func (l *avitoLane) Stop() error  { return nil }

// Send carries the stripe's out-of-band frames; its data comes by pull.
func (l *avitoLane) Send(b []byte) error { return l.tx.Send(b) }

func (l *avitoLane) Receive(cb func([]byte)) { l.cb = cb }

func (l *avitoLane) deliver(b []byte) {
	if l.cb != nil {
		l.cb(b)
	}
}

// IsConnected: the lane can send. What it hears comes in on the rx room,
// which the far side's lane sends into.
func (l *avitoLane) IsConnected() bool { return l.tx.connected.Load() }

func (l *avitoLane) Stats() transport.TransportStats {
	return transport.TransportStats{
		BytesSent:     l.tx.stats.bytesSent.Load(),
		BytesReceived: l.rx.stats.bytesRecv.Load(),
		PacketsSent:   l.tx.stats.opsSent.Load(),
		PacketsRecv:   l.rx.stats.opsRecv.Load(),
		Reconnects:    l.tx.stats.reconnects.Load() + l.rx.stats.reconnects.Load(),
		Connected:     l.tx.connected.Load(),
	}
}

// SetFrameSource implements transport.FramePuller.
func (l *avitoLane) SetFrameSource(src func(max int) [][]byte) { l.tx.pull.Store(&src) }

// Wake implements transport.LaneWaker.
func (l *avitoLane) Wake() {
	select {
	case l.tx.wake <- struct{}{}:
	default:
	}
}
