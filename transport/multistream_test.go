package transport

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// mockStream is a Transport we can flip up/down and inspect. Deliberately
// separate from batched_test.go's fakeTransport because the multi-stream tests
// need per-stream state (connectivity, peer liveness, send failures).
type mockStream struct {
	mu        sync.Mutex
	connected atomic.Bool
	lastRecv  atomic.Int64 // unix nanos; 0 = peer never heard from
	failSend  atomic.Bool
	stopped   atomic.Bool
	startErr  error
	sent      [][]byte
	cb        func([]byte)
}

// newAlive returns a connected stream whose peer was just heard from.
func newAlive() *mockStream {
	m := &mockStream{}
	m.connected.Store(true)
	m.lastRecv.Store(time.Now().UnixNano())
	return m
}

func (m *mockStream) Start() error {
	if m.startErr != nil {
		return m.startErr
	}
	return nil
}
func (m *mockStream) Stop() error { m.stopped.Store(true); m.connected.Store(false); return nil }
func (m *mockStream) Send(data []byte) error {
	if m.failSend.Load() {
		return fmt.Errorf("mock: send failure")
	}
	cp := append([]byte(nil), data...)
	m.mu.Lock()
	m.sent = append(m.sent, cp)
	m.mu.Unlock()
	return nil
}
func (m *mockStream) Receive(cb func([]byte)) {
	m.mu.Lock()
	m.cb = cb
	m.mu.Unlock()
}
func (m *mockStream) IsConnected() bool { return m.connected.Load() }
func (m *mockStream) Stats() TransportStats {
	st := TransportStats{Connected: m.IsConnected(), PacketsSent: uint64(m.sentCount())}
	if ns := m.lastRecv.Load(); ns != 0 {
		st.LastRecv = time.Unix(0, ns)
	}
	return st
}
func (m *mockStream) inject(data []byte) {
	m.mu.Lock()
	cb := m.cb
	m.mu.Unlock()
	if cb != nil {
		cb(data)
	}
}
func (m *mockStream) sentCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sent)
}

// tcpPacket builds a minimal IPv4/TCP packet for flow a:ap -> b:bp.
func tcpPacket(a, b [4]byte, ap, bp uint16, payload byte) []byte {
	pkt := make([]byte, 41)
	pkt[0] = 0x45
	binary.BigEndian.PutUint16(pkt[2:4], uint16(len(pkt)))
	pkt[8] = 64
	pkt[9] = 6
	copy(pkt[12:16], a[:])
	copy(pkt[16:20], b[:])
	binary.BigEndian.PutUint16(pkt[20:22], ap)
	binary.BigEndian.PutUint16(pkt[22:24], bp)
	pkt[40] = payload
	return pkt
}

var (
	clientIP = [4]byte{10, 10, 10, 2}
	serverIP = [4]byte{93, 184, 216, 34}
)

func startMS(t *testing.T, streams ...*mockStream) *MultiStreamTransport {
	t.Helper()
	inners := make([]Transport, len(streams))
	for i, s := range streams {
		inners[i] = s
	}
	ms := NewMultiStreamTransport(inners)
	if err := ms.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { ms.Stop() })
	return ms
}

// whichStream returns the index of the only stream that got packets since the
// given per-stream counts, failing if the packets were split.
func whichStream(t *testing.T, streams []*mockStream, before []int) int {
	t.Helper()
	got := -1
	for i, s := range streams {
		if s.sentCount() != before[i] {
			if got != -1 {
				t.Fatalf("flow split across streams %d and %d", got, i)
			}
			got = i
		}
	}
	if got == -1 {
		t.Fatal("no stream got the packets")
	}
	return got
}

func counts(streams []*mockStream) []int {
	out := make([]int, len(streams))
	for i, s := range streams {
		out[i] = s.sentCount()
	}
	return out
}

// Every packet of one connection, in both directions, must use one stream:
// spreading a connection over documents reorders its packets.
func TestMultiStreamPinsFlowToOneStream(t *testing.T) {
	streams := []*mockStream{newAlive(), newAlive(), newAlive()}
	ms := startMS(t, streams...)

	for port := uint16(40000); port < 40050; port++ {
		before := counts(streams)
		for i := 0; i < 20; i++ {
			if err := ms.Send(tcpPacket(clientIP, serverIP, port, 443, byte(i))); err != nil {
				t.Fatalf("Send: %v", err)
			}
			if err := ms.Send(tcpPacket(serverIP, clientIP, 443, port, byte(i))); err != nil {
				t.Fatalf("Send reverse: %v", err)
			}
		}
		whichStream(t, streams, before)
	}
}

// Many connections spread over all streams.
func TestMultiStreamSpreadsFlows(t *testing.T) {
	streams := []*mockStream{newAlive(), newAlive(), newAlive()}
	ms := startMS(t, streams...)

	const flows = 3000
	for port := 0; port < flows; port++ {
		if err := ms.Send(tcpPacket(clientIP, serverIP, uint16(20000+port), 443, 0)); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	for i, s := range streams {
		if n := s.sentCount(); n < flows/5 || n > flows/2 {
			t.Errorf("stream %d got %d of %d flows; want roughly a third", i, n, flows)
		}
	}
}

// A flow whose stream goes down moves to another stream, and comes back when
// the stream recovers.
func TestMultiStreamFlowFailsOverAndReturns(t *testing.T) {
	streams := []*mockStream{newAlive(), newAlive()}
	ms := startMS(t, streams...)
	pkt := tcpPacket(clientIP, serverIP, 51000, 443, 1)

	before := counts(streams)
	ms.Send(pkt)
	home := whichStream(t, streams, before)
	other := 1 - home

	streams[home].connected.Store(false)
	before = counts(streams)
	for i := 0; i < 10; i++ {
		if err := ms.Send(pkt); err != nil {
			t.Fatalf("Send with home stream down: %v", err)
		}
	}
	if got := whichStream(t, streams, before); got != other {
		t.Fatalf("flow went to stream %d while its home %d was down", got, home)
	}

	streams[home].connected.Store(true)
	before = counts(streams)
	ms.Send(pkt)
	if got := whichStream(t, streams, before); got != home {
		t.Fatalf("flow did not return home after recovery: went to %d", got)
	}
}

// A connected stream whose peer has gone silent (the peer dropped out of the
// document, or sits on another backend) must not get traffic while a stream
// with a live peer exists.
func TestMultiStreamAvoidsStreamWithSilentPeer(t *testing.T) {
	streams := []*mockStream{newAlive(), newAlive()}
	ms := startMS(t, streams...)
	now := time.Now()
	ms.now = func() time.Time { return now }

	streams[0].lastRecv.Store(now.Add(-2 * DefaultPeerTimeout).UnixNano())
	for port := uint16(1000); port < 1100; port++ {
		ms.Send(tcpPacket(clientIP, serverIP, port, 443, 0))
	}
	if n := streams[0].sentCount(); n != 0 {
		t.Fatalf("stream with a silent peer got %d packets", n)
	}
	if !ms.PeerAlive(1) || ms.PeerAlive(0) {
		t.Fatalf("PeerAlive = %v,%v; want false,true", ms.PeerAlive(0), ms.PeerAlive(1))
	}
}

// Before any peer is heard from (startup), connected streams are used anyway,
// still pinned per flow.
func TestMultiStreamUsesConnectedStreamsBeforePeerIsHeard(t *testing.T) {
	a, b := &mockStream{}, &mockStream{}
	a.connected.Store(true)
	b.connected.Store(true)
	streams := []*mockStream{a, b}
	ms := startMS(t, a, b)

	for port := uint16(2000); port < 2200; port++ {
		before := counts(streams)
		if err := ms.Send(tcpPacket(clientIP, serverIP, port, 443, 0)); err != nil {
			t.Fatalf("Send: %v", err)
		}
		ms.Send(tcpPacket(clientIP, serverIP, port, 443, 1))
		whichStream(t, streams, before)
	}
	if a.sentCount() == 0 || b.sentCount() == 0 {
		t.Fatalf("flows not spread before peer liveness is known: a=%d b=%d", a.sentCount(), b.sentCount())
	}
}

// A failed Send on the flow's stream goes to the next stream: the packet is
// not lost.
func TestMultiStreamFailoverOnSendError(t *testing.T) {
	streams := []*mockStream{newAlive(), newAlive()}
	ms := startMS(t, streams...)
	pkt := tcpPacket(clientIP, serverIP, 52000, 443, 1)

	before := counts(streams)
	ms.Send(pkt)
	home := whichStream(t, streams, before)

	streams[home].failSend.Store(true)
	before = counts(streams)
	if err := ms.Send(pkt); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := whichStream(t, streams, before); got == home {
		t.Fatal("packet stayed on the failing stream")
	}
}

func TestMultiStreamAllDown(t *testing.T) {
	a, b := &mockStream{}, &mockStream{}
	ms := startMS(t, a, b)
	if err := ms.Send(tcpPacket(clientIP, serverIP, 1, 2, 0)); err == nil {
		t.Fatal("Send succeeded with every stream down")
	}
}

// Non-IP payloads have no flow; they rotate over the streams.
func TestMultiStreamNonIPRoundRobin(t *testing.T) {
	a, b, c := newAlive(), newAlive(), newAlive()
	ms := startMS(t, a, b, c)
	for i := 0; i < 30; i++ {
		if err := ms.Send([]byte{0x00, byte(i)}); err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
	}
	if a.sentCount() != 10 || b.sentCount() != 10 || c.sentCount() != 10 {
		t.Fatalf("uneven rotation: a=%d b=%d c=%d", a.sentCount(), b.sentCount(), c.sentCount())
	}
}

func TestMultiStreamReceiveFanIn(t *testing.T) {
	a, b := newAlive(), newAlive()
	ms := startMS(t, a, b)

	var mu sync.Mutex
	var got [][]byte
	ms.Receive(func(data []byte) {
		mu.Lock()
		got = append(got, append([]byte(nil), data...))
		mu.Unlock()
	})
	a.inject([]byte("from-a"))
	b.inject([]byte("from-b"))

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || !bytes.Equal(got[0], []byte("from-a")) || !bytes.Equal(got[1], []byte("from-b")) {
		t.Fatalf("fan-in got %q", got)
	}
}

func TestMultiStreamStatsAggregation(t *testing.T) {
	a, b := newAlive(), &mockStream{}
	older := time.Now().Add(-time.Minute)
	b.lastRecv.Store(older.UnixNano())
	ms := startMS(t, a, b)

	st := ms.Stats()
	if !st.Connected {
		t.Error("Connected should be true while one stream is up")
	}
	if !st.LastRecv.Equal(time.Unix(0, a.lastRecv.Load())) {
		t.Errorf("LastRecv = %v, want the latest stream's", st.LastRecv)
	}
	a.connected.Store(false)
	if ms.Stats().Connected || ms.IsConnected() {
		t.Error("Connected should be false with every stream down")
	}
}

// One document failing to start must not take the tunnel down; all of them
// failing must.
func TestMultiStreamStartToleratesPartialFailure(t *testing.T) {
	good, bad := newAlive(), &mockStream{startErr: fmt.Errorf("doc gone")}
	ms := NewMultiStreamTransport([]Transport{bad, good})
	if err := ms.Start(); err != nil {
		t.Fatalf("Start with one good stream: %v", err)
	}
	if err := ms.Send(tcpPacket(clientIP, serverIP, 1, 2, 0)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	ms.Stop()

	b1, b2 := &mockStream{startErr: fmt.Errorf("x")}, &mockStream{startErr: fmt.Errorf("y")}
	ms = NewMultiStreamTransport([]Transport{b1, b2})
	if err := ms.Start(); err == nil {
		t.Fatal("Start succeeded with no stream started")
	}
}

func TestMultiStreamStopStopsEveryStream(t *testing.T) {
	a, b := newAlive(), newAlive()
	ms := startMS(t, a, b)
	if err := ms.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !a.stopped.Load() || !b.stopped.Load() {
		t.Fatal("Stop did not stop every inner stream")
	}
	if err := ms.Send(tcpPacket(clientIP, serverIP, 1, 2, 0)); err == nil {
		t.Fatal("Send succeeded after Stop")
	}
}

// A flow is sent back over the stream its packets arrive on, so when the peer
// moves a connection to another document the replies follow at once instead of
// going into the document the peer just lost.
func TestMultiStreamFlowFollowsPeer(t *testing.T) {
	streams := []*mockStream{newAlive(), newAlive()}
	ms := startMS(t, streams...)
	ms.Receive(func([]byte) {})
	now := time.Now()
	ms.now = func() time.Time { return now }
	out := tcpPacket(clientIP, serverIP, 53000, 443, 1)
	in := tcpPacket(serverIP, clientIP, 443, 53000, 2)

	before := counts(streams)
	ms.Send(out)
	home := whichStream(t, streams, before)
	other := 1 - home

	// The peer switched the flow to the other stream.
	streams[other].inject(in)
	before = counts(streams)
	ms.Send(out)
	if got := whichStream(t, streams, before); got != other {
		t.Fatalf("reply went to stream %d, want %d where the flow arrived", got, other)
	}

	// The followed stream goes down: back to the hash choice.
	streams[other].connected.Store(false)
	before = counts(streams)
	ms.Send(out)
	if got := whichStream(t, streams, before); got != home {
		t.Fatalf("with the followed stream down, went to %d, want %d", got, home)
	}
	streams[other].connected.Store(true)

	// A stale arrival no longer steers the flow.
	now = now.Add(2 * DefaultPeerTimeout)
	streams[0].lastRecv.Store(now.UnixNano())
	streams[1].lastRecv.Store(now.UnixNano())
	before = counts(streams)
	ms.Send(out)
	if got := whichStream(t, streams, before); got != home {
		t.Fatalf("stale arrival still steered the flow to %d", got)
	}
}
