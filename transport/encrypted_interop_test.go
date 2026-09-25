package transport

import (
	"bytes"
	"sync"
	"testing"
	"time"
)

// Interop between the iOS client stack in THIS branch and an exit node built
// from upstream master.
//
// Upstream runs Encrypted(Batched(raw)); this branch runs
// Encrypted(Adaptive(raw)), because the codec here self-negotiates instead of
// being picked by a --codec flag. The two only interoperate if
//
//	1. the batch framing is byte-identical (it is: framing.go matches upstream), and
//	2. the encryption layer sits in the SAME place in the stack — outermost, so
//	   each tunnel packet is sealed first and the codec batches the ciphertext.
//
// Point 2 is the one a refactor could silently break: move the wrapper inside
// the codec and everything still "works" locally between two peers from this
// branch, while every packet from an upstream node fails to authenticate. These
// tests pin it down.

// batchOnlyCodec models upstream's BatchedTransport on the wire: one batch frame
// per Send, decoded back into individual packets on receive. It uses the same
// encodeBatch/decodeBatch as the real code, so the bytes are the real bytes.
type batchOnlyCodec struct {
	Transport
	mu sync.Mutex
	cb func([]byte)
}

func (b *batchOnlyCodec) Send(data []byte) error {
	return b.Transport.Send(encodeBatch([][]byte{data}))
}

func (b *batchOnlyCodec) Receive(cb func([]byte)) {
	b.mu.Lock()
	b.cb = cb
	b.mu.Unlock()
	b.Transport.Receive(func(frame []byte) {
		pkts, err := decodeBatch(frame)
		if err != nil {
			return // a legacy/probe frame an upstream node would also discard
		}
		b.mu.Lock()
		f := b.cb
		b.mu.Unlock()
		if f == nil {
			return
		}
		for _, p := range pkts {
			if len(p) > 0 {
				f(p)
			}
		}
	})
}

// wire is a two-ended in-memory channel: what one side sends, the other receives.
type wire struct {
	mu   sync.Mutex
	peer *wire
	cb   func([]byte)
	sent [][]byte
}

func newWirePair() (*wire, *wire) {
	a, b := &wire{}, &wire{}
	a.peer, b.peer = b, a
	return a, b
}

func (w *wire) Start() error { return nil }
func (w *wire) Stop() error  { return nil }
func (w *wire) Send(data []byte) error {
	cp := append([]byte(nil), data...)
	w.mu.Lock()
	w.sent = append(w.sent, cp)
	w.mu.Unlock()

	w.peer.mu.Lock()
	cb := w.peer.cb
	w.peer.mu.Unlock()
	if cb != nil {
		cb(cp)
	}
	return nil
}
func (w *wire) Receive(cb func([]byte)) { w.mu.Lock(); w.cb = cb; w.mu.Unlock() }
func (w *wire) IsConnected() bool       { return true }
func (w *wire) Stats() TransportStats   { return TransportStats{Connected: true} }

func (w *wire) frames() [][]byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([][]byte, len(w.sent))
	copy(out, w.sent)
	return out
}

const (
	interopSecret = "correct horse battery staple"
	interopCtx    = "https://disk.yandex.ru/i/abcdef123456"
)

// buildInteropPair wires this branch's client stack to an upstream-style node.
func buildInteropPair(t *testing.T) (client, node Transport, clientWire, nodeWire *wire, stop func()) {
	t.Helper()
	cw, nw := newWirePair()

	adaptive := NewAdaptiveTransport(cw)
	adaptive.forceBatch = true // an upstream node is batch-only; skip the probe wait

	c, err := NewEncryptedTransport(adaptive, interopSecret, interopCtx, false)
	if err != nil {
		t.Fatalf("client encrypted transport: %v", err)
	}
	n, err := NewEncryptedTransport(&batchOnlyCodec{Transport: nw}, interopSecret, interopCtx, true)
	if err != nil {
		t.Fatalf("node encrypted transport: %v", err)
	}
	if err := c.Start(); err != nil {
		t.Fatalf("client start: %v", err)
	}
	return c, n, cw, nw, func() { _ = c.Stop() }
}

func waitForPacket(t *testing.T, got *[][]byte, mu *sync.Mutex, want []byte) bool {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		for _, p := range *got {
			if bytes.Equal(p, want) {
				mu.Unlock()
				return true
			}
		}
		mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// TestEncryptedInteropClientToUpstreamNode: a packet sent by this branch's
// client arrives intact at an upstream-style node, and never appears in the
// clear on the wire.
func TestEncryptedInteropClientToUpstreamNode(t *testing.T) {
	client, node, clientWire, _, stop := buildInteropPair(t)
	defer stop()

	var mu sync.Mutex
	var got [][]byte
	node.Receive(func(p []byte) {
		mu.Lock()
		got = append(got, append([]byte(nil), p...))
		mu.Unlock()
	})

	payload := []byte("client-to-exit payload, must survive intact")
	if err := client.Send(payload); err != nil {
		t.Fatalf("send: %v", err)
	}

	if !waitForPacket(t, &got, &mu, payload) {
		t.Fatal("upstream-style node never decoded the client's packet")
	}
	for _, f := range clientWire.frames() {
		if bytes.Contains(f, payload) {
			t.Fatal("plaintext payload found on the wire — encryption not applied")
		}
	}
}

// TestEncryptedInteropUpstreamNodeToClient: the reverse direction, which uses
// the other derived key, must work too.
func TestEncryptedInteropUpstreamNodeToClient(t *testing.T) {
	client, node, _, nodeWire, stop := buildInteropPair(t)
	defer stop()

	var mu sync.Mutex
	var got [][]byte
	client.Receive(func(p []byte) {
		mu.Lock()
		got = append(got, append([]byte(nil), p...))
		mu.Unlock()
	})
	node.Receive(func([]byte) {})

	payload := []byte("exit-to-client payload")
	if err := node.Send(payload); err != nil {
		t.Fatalf("node send: %v", err)
	}

	if !waitForPacket(t, &got, &mu, payload) {
		t.Fatal("client never decoded the node's packet")
	}
	for _, f := range nodeWire.frames() {
		if bytes.Contains(f, payload) {
			t.Fatal("plaintext payload found on the wire — encryption not applied")
		}
	}
}

// TestEncryptedInteropContextMismatch: the KDF context is derived from the
// document URL on both sides. If they disagree — a trailing slash, http vs
// https — the keys differ and nothing decodes. This is a silent, confusing
// failure in the field, so pin the behaviour.
func TestEncryptedInteropContextMismatch(t *testing.T) {
	cw, nw := newWirePair()

	adaptive := NewAdaptiveTransport(cw)
	adaptive.forceBatch = true
	client, err := NewEncryptedTransport(adaptive, interopSecret, interopCtx, false)
	if err != nil {
		t.Fatal(err)
	}
	// Same secret, context differs by one character.
	node, err := NewEncryptedTransport(&batchOnlyCodec{Transport: nw},
		interopSecret, interopCtx+"/", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	defer client.Stop()

	var mu sync.Mutex
	var got [][]byte
	node.Receive(func(p []byte) {
		mu.Lock()
		got = append(got, append([]byte(nil), p...))
		mu.Unlock()
	})

	payload := []byte("must not be readable by the wrong context")
	if err := client.Send(payload); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)

	mu.Lock()
	n := len(got)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("node decoded %d packets despite a different KDF context", n)
	}
}
