package transport

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"
)

type transientInnerTransport struct {
	fakeTransport
	muFailures sync.Mutex
	failures   int
	attempts   chan struct{}
}

func (f *transientInnerTransport) Send(data []byte) error {
	f.muFailures.Lock()
	if f.attempts != nil {
		select {
		case f.attempts <- struct{}{}:
		default:
		}
	}
	fail := f.failures != 0
	if f.failures > 0 {
		f.failures--
	}
	f.muFailures.Unlock()
	if fail {
		return errors.New("carrier temporarily unavailable")
	}
	return f.fakeTransport.Send(data)
}

type blockedInnerTransport struct {
	fakeTransport
	entered     chan struct{}
	release     chan struct{}
	enterOnce   sync.Once
	releaseOnce sync.Once
}

func (f *blockedInnerTransport) Send(data []byte) error {
	f.enterOnce.Do(func() { close(f.entered) })
	<-f.release
	return f.fakeTransport.Send(data)
}

func (f *blockedInnerTransport) Stop() error {
	f.releaseOnce.Do(func() { close(f.release) })
	return nil
}

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
func (f *fakeTransport) IsConnected() bool     { return true }
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
		t.Fatalf("expected exactly %d data packets without injected control records, got %d", n, len(pkts))
	}
}

func TestBatchedTransportStaysV2WithoutPeerAdvertisement(t *testing.T) {
	inner := &fakeTransport{}
	bt := NewBatchedTransport(inner)
	bt.lingerMs = 1
	if err := bt.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer bt.Stop()
	if err := bt.Send([]byte("legacy-compatible")); err != nil {
		t.Fatalf("send: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := inner.firstSent(); len(got) == 0 || got[0] != batchFormatVersion {
		t.Fatalf("wire version = %x, want v2", got)
	}
}

func TestBatchedTransportRejectsRetiredUnauthenticatedNegotiation(t *testing.T) {
	t.Setenv("OPENFLUX_EXPERIMENTAL_WIRE_V3", "1")
	inner := &fakeTransport{}
	bt := NewBatchedTransport(inner)
	bt.lingerMs = 1
	bt.Receive(func([]byte) {})
	if err := bt.Start(); err == nil {
		t.Fatal("unsafe prototype was allowed to start")
	}
	defer bt.Stop()

	if err := bt.Send([]byte("v3")); err == nil || inner.sendCount() != 0 {
		t.Fatal("unsafe prototype sent data")
	}
}

func TestBatchedTransportRejectsOversizedPacket(t *testing.T) {
	bt := NewBatchedTransport(&fakeTransport{})
	if err := bt.Send(make([]byte, 65536)); err == nil {
		t.Fatal("expected oversized packet error")
	}
}

func TestBatchedTransportWaitsForQueueCapacity(t *testing.T) {
	inner := &blockedInnerTransport{entered: make(chan struct{}), release: make(chan struct{})}
	bt := NewBatchedTransport(inner)
	bt.queue = make(chan []byte, 1)
	bt.maxBatchCount = 1
	bt.lingerMs = 0
	if err := bt.Start(); err != nil {
		t.Fatal(err)
	}
	defer bt.Stop()
	if err := bt.Send([]byte("first")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-inner.entered:
	case <-time.After(time.Second):
		t.Fatal("inner send did not start")
	}
	if err := bt.Send([]byte("second")); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- bt.Send([]byte("third")) }()
	select {
	case err := <-result:
		t.Fatalf("send returned before capacity was available: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	inner.releaseOnce.Do(func() { close(inner.release) })
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("send did not resume after capacity became available")
	}
	if got := bt.Stats().QueueWaits; got != 1 {
		t.Fatalf("queue waits = %d, want 1", got)
	}
}

func TestBatchedTransportStopUnblocksWaitingSend(t *testing.T) {
	inner := &blockedInnerTransport{entered: make(chan struct{}), release: make(chan struct{})}
	bt := NewBatchedTransport(inner)
	bt.queue = make(chan []byte, 1)
	bt.maxBatchCount = 1
	bt.lingerMs = 0
	if err := bt.Start(); err != nil {
		t.Fatal(err)
	}
	defer bt.Stop()
	if err := bt.Send([]byte("first")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-inner.entered:
	case <-time.After(time.Second):
		t.Fatal("inner send did not start")
	}
	if err := bt.Send([]byte("second")); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- bt.Send([]byte("third")) }()
	select {
	case err := <-result:
		t.Fatalf("send returned before stop: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	stopped := make(chan error, 1)
	go func() { stopped <- bt.Stop() }()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("stop blocked behind waiting send")
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("send reported success after stop")
		}
	case <-time.After(time.Second):
		t.Fatal("send did not unblock after stop")
	}
}

func TestBatchedTransportRetriesPendingBatchInOrder(t *testing.T) {
	inner := &transientInnerTransport{failures: 2}
	bt := NewBatchedTransport(inner)
	bt.maxBatchCount = 1
	bt.lingerMs = 0
	if err := bt.Start(); err != nil {
		t.Fatal(err)
	}
	defer bt.Stop()
	for _, p := range []string{"first", "second"} {
		if err := bt.Send([]byte(p)); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.After(time.Second)
	for inner.sendCount() < 2 {
		select {
		case <-deadline:
			t.Fatalf("delivered %d batches, want 2", inner.sendCount())
		case <-time.After(10 * time.Millisecond):
		}
	}
	inner.mu.Lock()
	sent := append([][]byte(nil), inner.sent...)
	inner.mu.Unlock()
	for i, want := range []string{"first", "second"} {
		pkts, err := decodeBatch(sent[i])
		if err != nil {
			t.Fatal(err)
		}
		if len(pkts) != 1 || string(pkts[0]) != want {
			t.Fatalf("batch %d = %q, want %q", i, pkts, want)
		}
	}
	if got := bt.Stats().SendRetries; got != 2 {
		t.Fatalf("send retries = %d, want 2", got)
	}
}

func TestBatchedTransportStopInterruptsCarrierRetry(t *testing.T) {
	inner := &transientInnerTransport{failures: -1, attempts: make(chan struct{}, 16)}
	bt := NewBatchedTransport(inner)
	bt.maxBatchCount = 1
	bt.lingerMs = 0
	if err := bt.Start(); err != nil {
		t.Fatal(err)
	}
	defer bt.Stop()
	if err := bt.Send([]byte("pending")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-inner.attempts:
		case <-time.After(time.Second):
			t.Fatal("pending batch was not retried")
		}
	}
	if err := bt.Stop(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-inner.attempts:
		t.Fatal("carrier retried after stop")
	case <-time.After(30 * time.Millisecond):
	}
}
