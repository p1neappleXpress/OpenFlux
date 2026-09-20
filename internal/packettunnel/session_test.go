package packettunnel

import (
	"bytes"
	"sync/atomic"
	"testing"
	"time"

	"openflux/transport"
)

type fakeRelay struct {
	connected     atomic.Bool
	starts, stops atomic.Int32
	cb            func([]byte)
}

func (f *fakeRelay) Start() error                    { f.starts.Add(1); f.connected.Store(true); return nil }
func (f *fakeRelay) Stop() error                     { f.stops.Add(1); f.connected.Store(false); return nil }
func (f *fakeRelay) Send([]byte) error               { return nil }
func (f *fakeRelay) Receive(cb func([]byte))         { f.cb = cb }
func (f *fakeRelay) IsConnected() bool               { return f.connected.Load() }
func (f *fakeRelay) Stats() transport.TransportStats { return transport.TransportStats{} }

func TestReconnectKeepsSessionAndReader(t *testing.T) {
	f := &fakeRelay{}
	s := New(f)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	f.connected.Store(false)
	if s.Connected() {
		t.Fatal("disconnect not visible")
	}
	buf := make([]byte, MaxPacketSize)
	if n := s.Read(buf); n != 0 {
		t.Fatalf("transient read = %d", n)
	}
	if s.Context().Err() != nil {
		t.Fatal("disconnect cancelled session")
	}
	f.connected.Store(true)
	want := bytes.Repeat([]byte{0x45}, 9000)
	f.cb(want)
	if n := s.Read(buf); n != len(want) || !bytes.Equal(buf[:n], want) {
		t.Fatal("reader did not survive reconnect")
	}
	if !s.Connected() || f.starts.Load() != 1 || f.stops.Load() != 0 {
		t.Fatal("reconnect replaced/stopped transport")
	}
}

func TestExplicitStopIsOnlyTerminalRead(t *testing.T) {
	f := &fakeRelay{}
	s := New(f)
	s.Start()
	s.Enqueue(nil)
	if n := s.Read(make([]byte, 20)); n != 0 {
		t.Fatalf("empty packet terminal: %d", n)
	}
	result := make(chan int, 1)
	go func() { result <- s.Read(make([]byte, 20)) }()
	s.Stop()
	s.Stop()
	select {
	case n := <-result:
		if n != -1 {
			t.Fatalf("stop read=%d", n)
		}
	case <-time.After(time.Second):
		t.Fatal("stop did not unblock reader")
	}
	s.Enqueue([]byte{1})
	if n := s.Read(nil); n != -1 {
		t.Fatalf("stopped read=%d", n)
	}
	if f.stops.Load() != 1 {
		t.Fatal("stop not idempotent")
	}
}

func TestWholePacketsAndBoundedQueue(t *testing.T) {
	f := &fakeRelay{}
	s := New(f)
	defer s.Stop()
	p := bytes.Repeat([]byte{7}, MaxPacketSize)
	s.Enqueue(p)
	short := bytes.Repeat([]byte{9}, 4096)
	if n := s.Read(short); n != 0 || short[0] != 9 {
		t.Fatal("packet silently truncated")
	}
	s.Enqueue(p)
	buf := make([]byte, MaxPacketSize)
	if n := s.Read(buf); n != len(p) || !bytes.Equal(buf, p) {
		t.Fatal("full IPv4 packet not preserved")
	}
	for i := 0; i < 1000; i++ {
		s.Enqueue(p)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.queuedBytes > QueueBytes || s.dropped == 0 {
		t.Fatal("queue memory is unbounded")
	}
}

func BenchmarkPacketSessionStartup(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s := New(&fakeRelay{})
		s.Start()
		s.Stop()
	}
}
