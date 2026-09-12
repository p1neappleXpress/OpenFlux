package transport

import (
	"bytes"
	"sync"
	"testing"
	"time"
)

// fakeTransport is a minimal in-process Transport used to test the batching
// wrapper. With loopback=true it immediately delivers whatever is Sent back to
// the registered receive callback (a perfect, ordered channel).
type fakeTransport struct {
	mu       sync.Mutex
	sent     [][]byte
	cb       func([]byte)
	loopback bool
}

func (f *fakeTransport) Start() error { return nil }
func (f *fakeTransport) Stop() error  { return nil }
func (f *fakeTransport) Send(data []byte) error {
	cp := append([]byte(nil), data...)
	f.mu.Lock()
	f.sent = append(f.sent, cp)
	cb := f.cb
	lb := f.loopback
	f.mu.Unlock()
	if lb && cb != nil {
		cb(cp)
	}
	return nil
}
func (f *fakeTransport) Receive(cb func([]byte)) {
	f.mu.Lock()
	f.cb = cb
	f.mu.Unlock()
}
func (f *fakeTransport) IsConnected() bool    { return true }
func (f *fakeTransport) Stats() TransportStats { return TransportStats{} }

func (f *fakeTransport) sendCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func (f *fakeTransport) firstSent() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		return nil
	}
	return f.sent[0]
}

func TestBatchedTransportRoundTripPreservesPacketsAndOrder(t *testing.T) {
	inner := &fakeTransport{loopback: true}
	bt := NewBatchedTransport(inner)
	bt.lingerMs = 10

	var mu sync.Mutex
	var got [][]byte
	bt.Receive(func(p []byte) {
		mu.Lock()
		got = append(got, append([]byte(nil), p...))
		mu.Unlock()
	})
	if err := bt.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer bt.Stop()

	want := [][]byte{[]byte("alpha"), []byte("bravo"), []byte("charlie"), {0xff, 0x00, 0x10}}
	for _, p := range want {
		if err := bt.Send(p); err != nil {
			t.Fatalf("send: %v", err)
		}
	}

	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(got) != len(want) {
		t.Fatalf("received %d packets, want %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Fatalf("packet %d = %v, want %v", i, got[i], want[i])
		}
	}
}

// The core optimization: a burst of packets must collapse into ONE inner
// transport message instead of one message per packet.
func TestBatchedTransportCoalescesBurstIntoOneMessage(t *testing.T) {
	inner := &fakeTransport{}
	bt := NewBatchedTransport(inner)
	bt.lingerMs = 50
	if err := bt.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer bt.Stop()

	const n = 5
	for i := 0; i < n; i++ {
		if err := bt.Send([]byte{byte(i), 0xAA, 0xBB}); err != nil {
			t.Fatalf("send: %v", err)
		}
	}

	time.Sleep(200 * time.Millisecond)

	if c := inner.sendCount(); c != 1 {
		t.Fatalf("expected 1 coalesced inner message, got %d", c)
	}
	pkts, err := decodeBatch(inner.firstSent())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(pkts) != n {
		t.Fatalf("expected %d packets in the batch, got %d", n, len(pkts))
	}
}
