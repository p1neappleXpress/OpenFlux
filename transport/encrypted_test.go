package transport

import (
	"bytes"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/flynn/noise"
)

// wireEnd is one end of a manual in-process channel. Send captures frames;
// the test decides when (and whether, and in what order) they reach the
// other end, which is what lets the tests exercise loss, reordering and
// replay deterministically.
type wireEnd struct {
	mu   sync.Mutex
	out  [][]byte
	cb   func([]byte)
	sent int
}

func (w *wireEnd) Start() error          { return nil }
func (w *wireEnd) Stop() error           { return nil }
func (w *wireEnd) IsConnected() bool     { return true }
func (w *wireEnd) Stats() TransportStats { return TransportStats{Connected: true} }

func (w *wireEnd) Receive(cb func([]byte)) {
	w.mu.Lock()
	w.cb = cb
	w.mu.Unlock()
}

func (w *wireEnd) Send(data []byte) error {
	w.mu.Lock()
	w.out = append(w.out, append([]byte(nil), data...))
	w.sent++
	w.mu.Unlock()
	return nil
}

// take returns the captured frames and clears the capture.
func (w *wireEnd) take() [][]byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := w.out
	w.out = nil
	return out
}

func (w *wireEnd) sentCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sent
}

// deliver hands a frame to whoever registered Receive on this end.
func (w *wireEnd) deliver(frame []byte) {
	w.mu.Lock()
	cb := w.cb
	w.mu.Unlock()
	if cb != nil {
		cb(frame)
	}
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// pair is a client and an exit node joined by two manual wires.
type pair struct {
	t          *testing.T
	clock      *fakeClock
	exitKey    noise.DHKey
	clientWire *wireEnd
	exitWire   *wireEnd
	client     *EncryptedTransport
	exit       *EncryptedTransport

	mu        sync.Mutex
	clientGot [][]byte
	exitGot   [][]byte
}

type pairOptions struct {
	clientPSK []byte
	exitPSK   []byte
	peerKey   []byte // what the client believes the exit public key is; nil = the real one
}

func newPair(t *testing.T, opts pairOptions) *pair {
	t.Helper()
	exitKey, err := GenerateStaticKey()
	if err != nil {
		t.Fatal(err)
	}
	p := &pair{
		t:          t,
		clock:      &fakeClock{t: time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)},
		exitKey:    exitKey,
		clientWire: &wireEnd{},
		exitWire:   &wireEnd{},
	}
	peerKey := opts.peerKey
	if peerKey == nil {
		peerKey = exitKey.Public
	}
	p.client, err = NewEncryptedTransport(p.clientWire, EncryptedConfig{
		Initiator:  true,
		PeerStatic: peerKey,
		PSK:        opts.clientPSK,
		Now:        p.clock.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	p.exit, err = NewEncryptedTransport(p.exitWire, EncryptedConfig{
		StaticKey: exitKey,
		PSK:       opts.exitPSK,
		Now:       p.clock.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	p.client.Receive(func(data []byte) {
		p.mu.Lock()
		p.clientGot = append(p.clientGot, append([]byte(nil), data...))
		p.mu.Unlock()
	})
	p.exit.Receive(func(data []byte) {
		p.mu.Lock()
		p.exitGot = append(p.exitGot, append([]byte(nil), data...))
		p.mu.Unlock()
	})
	return p
}

// pump moves every captured frame from one end to the other, in order.
func (p *pair) pump(from, to *wireEnd) int {
	frames := from.take()
	for _, f := range frames {
		to.deliver(f)
	}
	return len(frames)
}

// pumpAll shuttles frames both ways until the wires are quiet.
func (p *pair) pumpAll() {
	for i := 0; i < 100; i++ {
		if p.pump(p.clientWire, p.exitWire)+p.pump(p.exitWire, p.clientWire) == 0 {
			return
		}
	}
	p.t.Fatal("wires never went quiet")
}

func (p *pair) received(side *[][]byte) [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([][]byte(nil), (*side)...)
}

func (p *pair) exitReceived() [][]byte   { return p.received(&p.exitGot) }
func (p *pair) clientReceived() [][]byte { return p.received(&p.clientGot) }

func assertNoPlaintext(t *testing.T, frames [][]byte, plaintext []byte) {
	t.Helper()
	for i, f := range frames {
		if bytes.Contains(f, plaintext) {
			t.Fatalf("frame %d carries the plaintext", i)
		}
	}
}

func assertPackets(t *testing.T, got [][]byte, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("received %d packets %q, want %d %q", len(got), got, len(want), want)
	}
	for i := range want {
		if string(got[i]) != want[i] {
			t.Fatalf("packet %d is %q, want %q", i, got[i], want[i])
		}
	}
}

func TestEncryptedRoundTripThroughHandshake(t *testing.T) {
	p := newPair(t, pairOptions{})

	if err := p.client.Send([]byte("hello exit")); err != nil {
		t.Fatal(err)
	}
	// Before any session exists the client sends only its handshake
	// initiation and holds the data back.
	init := p.clientWire.take()
	if len(init) != 1 {
		t.Fatalf("client sent %d frames before the handshake, want 1", len(init))
	}
	assertNoPlaintext(t, init, []byte("hello exit"))
	p.exitWire.deliver(init[0])

	resp := p.exitWire.take()
	if len(resp) != 1 {
		t.Fatalf("exit sent %d frames in response to the initiation, want 1", len(resp))
	}
	assertNoPlaintext(t, resp, []byte("hello exit"))
	p.clientWire.deliver(resp[0])

	// The held-back packet goes out as soon as the session exists.
	data := p.clientWire.take()
	if len(data) != 1 {
		t.Fatalf("client sent %d frames after the handshake, want 1", len(data))
	}
	assertNoPlaintext(t, data, []byte("hello exit"))
	p.exitWire.deliver(data[0])
	assertPackets(t, p.exitReceived(), "hello exit")

	if err := p.exit.Send([]byte("hello client")); err != nil {
		t.Fatal(err)
	}
	reply := p.exitWire.take()
	assertNoPlaintext(t, reply, []byte("hello client"))
	if len(reply) != 1 {
		t.Fatalf("exit sent %d frames, want 1", len(reply))
	}
	p.clientWire.deliver(reply[0])
	assertPackets(t, p.clientReceived(), "hello client")

	// Steady state: one frame per packet, both ways.
	if err := p.client.Send([]byte("second")); err != nil {
		t.Fatal(err)
	}
	p.pumpAll()
	assertPackets(t, p.exitReceived(), "hello exit", "second")
}

func TestEncryptedRejectsWrongExitKey(t *testing.T) {
	other, err := GenerateStaticKey()
	if err != nil {
		t.Fatal(err)
	}
	p := newPair(t, pairOptions{peerKey: other.Public})

	if err := p.client.Send([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	p.pumpAll()
	if n := p.exitWire.sentCount(); n != 0 {
		t.Fatalf("exit answered %d frames to a client with the wrong key, want silence", n)
	}
	if got := p.exitReceived(); len(got) != 0 {
		t.Fatalf("exit delivered %d packets, want 0", len(got))
	}
}

func TestEncryptedRejectsPSKMismatch(t *testing.T) {
	pskA := bytes.Repeat([]byte{1}, 32)
	pskB := bytes.Repeat([]byte{2}, 32)
	cases := []struct {
		name               string
		clientPSK, exitPSK []byte
	}{
		{"different secrets", pskA, pskB},
		{"client has a secret, exit has none", pskA, nil},
		{"exit has a secret, client has none", nil, pskA},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newPair(t, pairOptions{clientPSK: tc.clientPSK, exitPSK: tc.exitPSK})
			if err := p.client.Send([]byte("hello")); err != nil {
				t.Fatal(err)
			}
			p.pumpAll()
			if n := p.exitWire.sentCount(); n != 0 {
				t.Fatalf("exit answered %d frames despite the PSK mismatch, want silence", n)
			}
			if got := p.exitReceived(); len(got) != 0 {
				t.Fatalf("exit delivered %d packets, want 0", len(got))
			}
		})
	}

	t.Run("matching secrets work", func(t *testing.T) {
		p := newPair(t, pairOptions{clientPSK: pskA, exitPSK: pskA})
		if err := p.client.Send([]byte("hello")); err != nil {
			t.Fatal(err)
		}
		p.pumpAll()
		assertPackets(t, p.exitReceived(), "hello")
	})
}

// establish runs a handshake with a probe packet each way and forgets the
// probes, leaving a pair with a confirmed session on both sides.
func (p *pair) establish() {
	p.t.Helper()
	if err := p.client.Send([]byte("probe")); err != nil {
		p.t.Fatal(err)
	}
	p.pumpAll()
	assertPackets(p.t, p.exitReceived(), "probe")
	if err := p.exit.Send([]byte("probe back")); err != nil {
		p.t.Fatal(err)
	}
	p.pumpAll()
	assertPackets(p.t, p.clientReceived(), "probe back")
	p.mu.Lock()
	p.clientGot, p.exitGot = nil, nil
	p.mu.Unlock()
}

// clientFrames sends the packets from the client and returns one captured
// frame per packet, undelivered.
func (p *pair) clientFrames(packets ...string) [][]byte {
	p.t.Helper()
	for _, pkt := range packets {
		if err := p.client.Send([]byte(pkt)); err != nil {
			p.t.Fatal(err)
		}
	}
	frames := p.clientWire.take()
	if len(frames) != len(packets) {
		p.t.Fatalf("client produced %d frames for %d packets", len(frames), len(packets))
	}
	return frames
}

func TestEncryptedRejectsTamperedData(t *testing.T) {
	p := newPair(t, pairOptions{})
	p.establish()
	frames := p.clientFrames("payload", "payload")

	body := append([]byte(nil), frames[0]...)
	body[len(body)-1] ^= 1
	p.exitWire.deliver(body)

	header := append([]byte(nil), frames[1]...)
	header[encryptedHeader+4+7] ^= 1 // low byte of the counter
	p.exitWire.deliver(header)

	if got := p.exitReceived(); len(got) != 0 {
		t.Fatalf("exit delivered %d tampered packets", len(got))
	}
}

func TestEncryptedReplayWindow(t *testing.T) {
	p := newPair(t, pairOptions{})
	p.establish()
	frames := p.clientFrames("a", "b", "c")

	// Reordered delivery is fine: the tunnel carries TCP.
	p.exitWire.deliver(frames[0])
	p.exitWire.deliver(frames[2])
	p.exitWire.deliver(frames[1])
	assertPackets(t, p.exitReceived(), "a", "c", "b")

	// A replayed frame, whether the newest or an older one, is dropped.
	p.exitWire.deliver(frames[2])
	p.exitWire.deliver(frames[0])
	assertPackets(t, p.exitReceived(), "a", "c", "b")
}

func frameType(frame []byte) byte { return frame[4] }

// receiverIndex is the session index a data frame is addressed to.
func receiverIndex(frame []byte) []byte { return frame[encryptedHeader : encryptedHeader+4] }

// restartExit replaces the exit transport with a fresh one on the same wire
// and static key, as if the exit process had restarted and lost its
// sessions.
func (p *pair) restartExit() {
	p.t.Helper()
	exit, err := NewEncryptedTransport(p.exitWire, EncryptedConfig{
		StaticKey: p.exitKey,
		Now:       p.clock.now,
	})
	if err != nil {
		p.t.Fatal(err)
	}
	exit.Receive(func(data []byte) {
		p.mu.Lock()
		p.exitGot = append(p.exitGot, append([]byte(nil), data...))
		p.mu.Unlock()
	})
	p.exit = exit
}

func TestEncryptedRekeysAfterRekeyTime(t *testing.T) {
	p := newPair(t, pairOptions{})
	p.establish()
	old := p.clientFrames("old-1", "old-2") // held back, still on the first session

	p.clock.advance(rekeyAfterTime + time.Second)
	if err := p.client.Send([]byte("trigger")); err != nil {
		t.Fatal(err)
	}
	frames := p.clientWire.take()
	if len(frames) != 2 || frameType(frames[0]) != msgHandshakeInit || frameType(frames[1]) != msgData {
		t.Fatalf("an aged session must trigger a handshake while data keeps flowing; got %d frames", len(frames))
	}
	if !bytes.Equal(receiverIndex(frames[1]), receiverIndex(old[0])) {
		t.Fatal("data sent during the rekey left the old session before the new one existed")
	}
	p.exitWire.deliver(frames[0])
	p.exitWire.deliver(frames[1])
	assertPackets(t, p.exitReceived(), "trigger")
	if n := p.pump(p.exitWire, p.clientWire); n != 1 {
		t.Fatalf("exit sent %d frames back, want the handshake response only", n)
	}

	fresh := p.clientFrames("new-1")
	if bytes.Equal(receiverIndex(fresh[0]), receiverIndex(old[0])) {
		t.Fatal("client kept sending on the old session after the handshake completed")
	}
	p.exitWire.deliver(fresh[0])
	assertPackets(t, p.exitReceived(), "trigger", "new-1")

	// The old session stays valid until rejectAfterTime after it was made.
	p.exitWire.deliver(old[0])
	assertPackets(t, p.exitReceived(), "trigger", "new-1", "old-1")
	p.clock.advance(rejectAfterTime - rekeyAfterTime)
	p.exitWire.deliver(old[1])
	assertPackets(t, p.exitReceived(), "trigger", "new-1", "old-1")
}

func TestEncryptedRecoversFromExitRestart(t *testing.T) {
	p := newPair(t, pairOptions{})
	p.establish()
	p.restartExit()
	exitSentBefore := p.exitWire.sentCount()

	if err := p.client.Send([]byte("lost")); err != nil {
		t.Fatal(err)
	}
	p.pumpAll()
	if n := p.clientWire.sentCount(); n != 3 {
		t.Fatalf("client sent %d frames, want 3 (handshake init, probe, lost)", n)
	}
	if n := p.exitWire.sentCount() - exitSentBefore; n != 0 {
		t.Fatalf("restarted exit answered %d frames to an unknown session", n)
	}

	// Silence after our own sends is the only signal that the exit is gone.
	p.clock.advance(keepaliveTimeout + rekeyTimeout)
	if err := p.client.Send([]byte("retry")); err != nil {
		t.Fatal(err)
	}
	frames := p.clientWire.take()
	if len(frames) != 2 || frameType(frames[0]) != msgHandshakeInit {
		t.Fatalf("client did not start a handshake after %v of silence; sent %d frames", keepaliveTimeout+rekeyTimeout, len(frames))
	}
	for _, f := range frames {
		p.exitWire.deliver(f)
	}
	p.pumpAll()
	if err := p.client.Send([]byte("after")); err != nil {
		t.Fatal(err)
	}
	p.pumpAll()
	assertPackets(t, p.exitReceived(), "after")

	if err := p.exit.Send([]byte("welcome back")); err != nil {
		t.Fatal(err)
	}
	p.pumpAll()
	assertPackets(t, p.clientReceived(), "welcome back")
}

func TestEncryptedRetransmitsHandshake(t *testing.T) {
	p := newPair(t, pairOptions{})
	if err := p.client.Send([]byte("x")); err != nil {
		t.Fatal(err)
	}
	lost := p.clientWire.take()
	if len(lost) != 1 {
		t.Fatalf("client sent %d frames, want the initiation", len(lost))
	}

	p.client.tick()
	if n := len(p.clientWire.take()); n != 0 {
		t.Fatalf("client retransmitted %d frames before the timeout", n)
	}
	p.clock.advance(rekeyTimeout)
	p.client.tick()
	again := p.clientWire.take()
	if len(again) != 1 || !bytes.Equal(again[0], lost[0]) {
		t.Fatalf("expected the identical initiation to be resent, got %d frames", len(again))
	}
	p.exitWire.deliver(again[0])
	p.pumpAll()
	assertPackets(t, p.exitReceived(), "x")
}

func TestEncryptedGivesUpHandshakeAfterAttemptTime(t *testing.T) {
	p := newPair(t, pairOptions{})
	if err := p.client.Send([]byte("stale")); err != nil {
		t.Fatal(err)
	}
	p.clientWire.take()

	resent := 0
	for elapsed := time.Duration(0); elapsed < rekeyAttemptTime; elapsed += rekeyTimeout {
		p.clock.advance(rekeyTimeout)
		p.client.tick()
		resent += len(p.clientWire.take())
	}
	if want := int(rekeyAttemptTime/rekeyTimeout) - 1; resent != want {
		t.Fatalf("resent %d times, want %d", resent, want)
	}
	p.clock.advance(rekeyTimeout)
	p.client.tick()
	if n := len(p.clientWire.take()); n != 0 {
		t.Fatalf("client kept retransmitting after giving up: %d frames", n)
	}

	// The next packet starts over with a fresh handshake; the stale packet
	// was dropped, the tunnel's TCP will have retransmitted it long ago.
	if err := p.client.Send([]byte("fresh")); err != nil {
		t.Fatal(err)
	}
	p.pumpAll()
	assertPackets(t, p.exitReceived(), "fresh")
}

func TestEncryptedStagesPacketsDuringHandshake(t *testing.T) {
	t.Run("kept in order", func(t *testing.T) {
		p := newPair(t, pairOptions{})
		for _, pkt := range []string{"p1", "p2", "p3"} {
			if err := p.client.Send([]byte(pkt)); err != nil {
				t.Fatal(err)
			}
		}
		if n := p.clientWire.sentCount(); n != 1 {
			t.Fatalf("client put %d frames on the wire without a session, want the initiation only", n)
		}
		p.pumpAll()
		assertPackets(t, p.exitReceived(), "p1", "p2", "p3")
	})

	t.Run("oldest dropped beyond the cap", func(t *testing.T) {
		p := newPair(t, pairOptions{})
		var want []string
		for i := 0; i < maxStagedPackets+3; i++ {
			pkt := fmt.Sprintf("p%03d", i)
			if i >= 3 {
				want = append(want, pkt)
			}
			if err := p.client.Send([]byte(pkt)); err != nil {
				t.Fatal(err)
			}
		}
		p.pumpAll()
		assertPackets(t, p.exitReceived(), want...)
	})
}

// newClient makes another initiator for the same exit on its own wire.
func (p *pair) newClient() (*EncryptedTransport, *wireEnd, func() [][]byte) {
	p.t.Helper()
	wire := &wireEnd{}
	c, err := NewEncryptedTransport(wire, EncryptedConfig{
		Initiator:  true,
		PeerStatic: p.exitKey.Public,
		Now:        p.clock.now,
	})
	if err != nil {
		p.t.Fatal(err)
	}
	var mu sync.Mutex
	var got [][]byte
	c.Receive(func(data []byte) {
		mu.Lock()
		got = append(got, append([]byte(nil), data...))
		mu.Unlock()
	})
	return c, wire, func() [][]byte {
		mu.Lock()
		defer mu.Unlock()
		return append([][]byte(nil), got...)
	}
}

func TestEncryptedLastHandshakeWins(t *testing.T) {
	p := newPair(t, pairOptions{})
	p.establish() // client A
	toA := p.clientFrames("from A")
	p.exitWire.deliver(toA[0])

	b, bWire, bReceived := p.newClient()
	if err := b.Send([]byte("from B")); err != nil {
		t.Fatal(err)
	}
	init := bWire.take()
	p.exitWire.deliver(init[0])
	resp := p.exitWire.take()
	if len(resp) != 1 {
		t.Fatalf("exit sent %d frames for the second handshake, want 1", len(resp))
	}

	// B has not proven its keys yet: replies still go to A.
	if err := p.exit.Send([]byte("still to A")); err != nil {
		t.Fatal(err)
	}
	reply := p.exitWire.take()
	p.clientWire.deliver(reply[0])
	assertPackets(t, p.clientReceived(), "still to A")

	bWire.deliver(resp[0])
	data := bWire.take()
	p.exitWire.deliver(data[0])
	assertPackets(t, p.exitReceived(), "from A", "from B")

	// Now B is the peer of this document; A only gets what was in flight.
	if err := p.exit.Send([]byte("to B")); err != nil {
		t.Fatal(err)
	}
	reply = p.exitWire.take()
	p.clientWire.deliver(reply[0])
	bWire.deliver(reply[0])
	assertPackets(t, p.clientReceived(), "still to A")
	assertPackets(t, bReceived(), "to B")

	// A's session is still accepted for receiving until it expires.
	late := p.clientFrames("late from A")
	p.exitWire.deliver(late[0])
	assertPackets(t, p.exitReceived(), "from A", "from B", "late from A")
}

func TestEncryptedIgnoresGarbageAndV1(t *testing.T) {
	p := newPair(t, pairOptions{})
	p.establish()
	clientSent, exitSent := p.clientWire.sentCount(), p.exitWire.sentCount()

	v1 := append([]byte{'O', 'F', 'X', 1, 0}, bytes.Repeat([]byte{7}, 40)...)
	unknownType := append([]byte{'O', 'F', 'X', 2, 9}, bytes.Repeat([]byte{7}, 40)...)
	shortInit := []byte{'O', 'F', 'X', 2, msgHandshakeInit, 1, 2, 3}
	shortData := []byte{'O', 'F', 'X', 2, msgData, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	unknownSession := append([]byte{'O', 'F', 'X', 2, msgData, 0xde, 0xad, 0xbe, 0xef, 0, 0, 0, 0, 0, 0, 0, 0}, bytes.Repeat([]byte{7}, 20)...)
	bogusInit := append([]byte{'O', 'F', 'X', 2, msgHandshakeInit, 1, 2, 3, 4}, bytes.Repeat([]byte{7}, noiseMsgSize)...)
	bogusResp := append([]byte{'O', 'F', 'X', 2, msgHandshakeResp, 1, 2, 3, 4, 5, 6, 7, 8}, bytes.Repeat([]byte{7}, noiseMsgSize)...)
	for _, frame := range [][]byte{nil, {}, {'O'}, {'O', 'F', 'X'}, v1, unknownType, shortInit, shortData, unknownSession, bogusInit, bogusResp} {
		p.clientWire.deliver(frame)
		p.exitWire.deliver(frame)
	}
	if got := p.clientReceived(); len(got) != 0 {
		t.Fatalf("client delivered %d packets from garbage", len(got))
	}
	if got := p.exitReceived(); len(got) != 0 {
		t.Fatalf("exit delivered %d packets from garbage", len(got))
	}
	if n := p.clientWire.sentCount() - clientSent; n != 0 {
		t.Fatalf("client reacted to garbage with %d frames", n)
	}
	if n := p.exitWire.sentCount() - exitSent; n != 0 {
		t.Fatalf("exit reacted to garbage with %d frames", n)
	}

	// The session is intact afterwards.
	if err := p.client.Send([]byte("still fine")); err != nil {
		t.Fatal(err)
	}
	p.pumpAll()
	assertPackets(t, p.exitReceived(), "still fine")
}

func TestEncryptedResponderNeverInitiates(t *testing.T) {
	p := newPair(t, pairOptions{})
	if err := p.exit.Send([]byte("unsolicited")); err == nil {
		t.Fatal("exit sent without a session")
	}
	if n := p.exitWire.sentCount(); n != 0 {
		t.Fatalf("exit put %d frames on the wire without a session", n)
	}

	// An answered but unconfirmed handshake is not a session to send on yet.
	if err := p.client.Send([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	p.pump(p.clientWire, p.exitWire)
	if err := p.exit.Send([]byte("too early")); err == nil {
		t.Fatal("exit sent on an unconfirmed session")
	}
	if n := p.exitWire.sentCount(); n != 1 {
		t.Fatalf("exit sent %d frames, want the handshake response only", n)
	}
}

func TestEncryptedRekeysOnCounterLimit(t *testing.T) {
	p := newPair(t, pairOptions{})
	p.establish()

	p.client.current.send.SetNonce(rekeyAfterMessages)
	if err := p.client.Send([]byte("at the rekey limit")); err != nil {
		t.Fatal(err)
	}
	frames := p.clientWire.take()
	if len(frames) != 2 || frameType(frames[0]) != msgHandshakeInit || frameType(frames[1]) != msgData {
		t.Fatalf("reaching the rekey limit must start a handshake while data keeps flowing; got %d frames", len(frames))
	}
	for _, f := range frames {
		p.exitWire.deliver(f)
	}
	p.pumpAll()
	assertPackets(t, p.exitReceived(), "at the rekey limit")

	// A session at the hard limit is unusable: the packet waits for the
	// next handshake instead of going out under a repeated nonce.
	p.client.current.send.SetNonce(rejectAfterMessages)
	if err := p.client.Send([]byte("past the hard limit")); err != nil {
		t.Fatal(err)
	}
	frames = p.clientWire.take()
	if len(frames) != 1 || frameType(frames[0]) != msgHandshakeInit {
		t.Fatalf("expected only a handshake initiation, got %d frames", len(frames))
	}
	p.exitWire.deliver(frames[0])
	p.pumpAll()
	assertPackets(t, p.exitReceived(), "at the rekey limit", "past the hard limit")
}

func TestEncryptedBoundsUnconfirmedSessions(t *testing.T) {
	p := newPair(t, pairOptions{})
	p.establish()

	// A flood of initiations from strangers must not push out the session
	// that is actually in use.
	for i := 0; i < 4*maxResponderSessions; i++ {
		c, wire, _ := p.newClient()
		if err := c.Send([]byte("flood")); err != nil {
			t.Fatal(err)
		}
		for _, f := range wire.take() {
			p.exitWire.deliver(f)
		}
		p.exitWire.take()
	}
	p.exit.mu.Lock()
	n := len(p.exit.sessions)
	p.exit.mu.Unlock()
	if n > maxResponderSessions {
		t.Fatalf("exit holds %d sessions, want at most %d", n, maxResponderSessions)
	}

	if err := p.client.Send([]byte("still here")); err != nil {
		t.Fatal(err)
	}
	p.pumpAll()
	assertPackets(t, p.exitReceived(), "still here")
	if err := p.exit.Send([]byte("still yours")); err != nil {
		t.Fatal(err)
	}
	p.pumpAll()
	assertPackets(t, p.clientReceived(), "still yours")
}

func TestEncryptedStartAndStop(t *testing.T) {
	p := newPair(t, pairOptions{})
	if err := p.exit.Start(); err != nil {
		t.Fatal(err)
	}
	if n := p.exitWire.sentCount(); n != 0 {
		t.Fatalf("exit sent %d frames on start", n)
	}
	if err := p.client.Start(); err != nil {
		t.Fatal(err)
	}
	if n := p.clientWire.sentCount(); n != 1 {
		t.Fatalf("client sent %d frames on start, want its handshake initiation", n)
	}
	p.pumpAll()
	if err := p.client.Send([]byte("ready")); err != nil {
		t.Fatal(err)
	}
	p.pumpAll()
	assertPackets(t, p.exitReceived(), "ready")

	done := make(chan struct{})
	go func() {
		_ = p.client.Stop()
		_ = p.client.Stop()
		_ = p.exit.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return")
	}
}

func TestEncryptedKeepaliveAnswersSilence(t *testing.T) {
	t.Run("exit keeps the client from rekeying needlessly", func(t *testing.T) {
		p := newPair(t, pairOptions{})
		p.establish()

		// The client's last packet needs no reply (think: the final ACK of a
		// closed connection). The exit must still show signs of life.
		if err := p.client.Send([]byte("final ack")); err != nil {
			t.Fatal(err)
		}
		p.pumpAll()
		p.clock.advance(keepaliveTimeout - time.Second)
		p.exit.tick()
		if n := len(p.exitWire.take()); n != 0 {
			t.Fatalf("exit sent %d frames before the keepalive timeout", n)
		}
		p.clock.advance(time.Second)
		p.exit.tick()
		keepalive := p.exitWire.take()
		if len(keepalive) != 1 || frameType(keepalive[0]) != msgData {
			t.Fatalf("exit sent %d frames at the keepalive timeout, want one data frame", len(keepalive))
		}
		p.exit.tick()
		if n := len(p.exitWire.take()); n != 0 {
			t.Fatalf("exit repeated the keepalive %d times", n)
		}
		p.clientWire.deliver(keepalive[0])
		if got := p.clientReceived(); len(got) != 0 {
			t.Fatalf("keepalive was delivered to the tunnel as %q", got)
		}

		// Well past the silence threshold, the client still trusts the session.
		p.clock.advance(keepaliveTimeout + rekeyTimeout)
		frames := p.clientFrames("next request")
		if frameType(frames[0]) != msgData {
			t.Fatal("client started a handshake although the exit answered with a keepalive")
		}
	})

	t.Run("client answers too", func(t *testing.T) {
		p := newPair(t, pairOptions{})
		p.establish()
		if err := p.exit.Send([]byte("push")); err != nil {
			t.Fatal(err)
		}
		p.pumpAll()
		p.clock.advance(keepaliveTimeout)
		p.client.tick()
		keepalive := p.clientWire.take()
		if len(keepalive) != 1 || frameType(keepalive[0]) != msgData {
			t.Fatalf("client sent %d frames at the keepalive timeout, want one data frame", len(keepalive))
		}
		p.exitWire.deliver(keepalive[0])
		if got := p.exitReceived(); len(got) != 0 {
			t.Fatalf("keepalive was delivered to the tunnel as %q", got)
		}
	})

	t.Run("no keepalive while talking", func(t *testing.T) {
		p := newPair(t, pairOptions{})
		p.establish()
		if err := p.client.Send([]byte("request")); err != nil {
			t.Fatal(err)
		}
		p.pumpAll()
		if err := p.exit.Send([]byte("reply")); err != nil {
			t.Fatal(err)
		}
		p.pumpAll()
		p.clock.advance(keepaliveTimeout)
		p.exit.tick()
		if n := len(p.exitWire.take()); n != 0 {
			t.Fatalf("exit sent %d keepalives although its reply was the last word", n)
		}
	})
}

func TestEncryptedConcurrentTraffic(t *testing.T) {
	p := newPair(t, pairOptions{})
	p.establish()

	const senders, perSender = 4, 200
	var wg sync.WaitGroup
	stopPump := make(chan struct{})
	var pumpWG sync.WaitGroup
	pumpWG.Add(1)
	go func() {
		defer pumpWG.Done()
		for {
			select {
			case <-stopPump:
				p.pumpAll()
				return
			default:
				p.pump(p.clientWire, p.exitWire)
				p.pump(p.exitWire, p.clientWire)
			}
		}
	}()
	for i := 0; i < senders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < perSender; j++ {
				if err := p.client.Send([]byte(fmt.Sprintf("c%d-%d", i, j))); err != nil {
					t.Error(err)
				}
				if err := p.exit.Send([]byte(fmt.Sprintf("e%d-%d", i, j))); err != nil {
					t.Error(err)
				}
			}
		}(i)
	}
	wg.Wait()
	close(stopPump)
	pumpWG.Wait()

	if got := len(p.exitReceived()); got != senders*perSender {
		t.Fatalf("exit received %d packets, want %d", got, senders*perSender)
	}
	if got := len(p.clientReceived()); got != senders*perSender {
		t.Fatalf("client received %d packets, want %d", got, senders*perSender)
	}
}
