package fluxcore

import (
	"sync"
	"testing"
	"time"
)

// recCarrier records what the engine sends through it, for deterministic
// routing tests (no timing involved).
type recCarrier struct {
	name string
	mu   sync.Mutex
	sent [][]byte
	conn bool
}

func newRec(name string) *recCarrier { return &recCarrier{name: name, conn: true} }
func (c *recCarrier) Name() string   { return c.name }
func (c *recCarrier) Start() error   { c.SetConn(true); return nil }
func (c *recCarrier) Stop() error    { c.SetConn(false); return nil }
func (c *recCarrier) Connected() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.conn }
func (c *recCarrier) SetConn(v bool)  { c.mu.Lock(); c.conn = v; c.mu.Unlock() }
func (c *recCarrier) SetInbound(func([]byte)) {}
func (c *recCarrier) Send(b []byte) error {
	c.mu.Lock()
	c.sent = append(c.sent, append([]byte(nil), b...))
	c.mu.Unlock()
	return nil
}
func (c *recCarrier) dataFrames() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, f := range c.sent {
		if len(f) > 0 && f[0] == frameData {
			n++
		}
	}
	return n
}

func newEngineForTest() (*Engine, *Registry) {
	reg := NewRegistry()
	bus := NewBus(20)
	brain := NewBrain(reg, bus, DefaultSelector(), nil)
	return NewEngine(reg, bus, brain), reg
}

func TestEngineRoutesDataToActiveCarrier(t *testing.T) {
	eng, _ := newEngineForTest()
	ca, cb := newRec("yandex"), newRec("max")
	ra := eng.Add(RouteDNA{Transport: "yandex", Exit: "tr"}, ca)
	eng.Add(RouteDNA{Transport: "max", Exit: "tr"}, cb)
	ra.SetState(StateActive)

	if err := eng.Send([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if ca.dataFrames() != 1 {
		t.Fatalf("active carrier should have received the data frame, got %d", ca.dataFrames())
	}
	if cb.dataFrames() != 0 {
		t.Fatalf("standby carrier must not carry traffic, got %d", cb.dataFrames())
	}
}

func TestEngineSendErrsWithoutActive(t *testing.T) {
	eng, _ := newEngineForTest()
	eng.Add(RouteDNA{Transport: "yandex"}, newRec("y")) // left in Unknown state
	if err := eng.Send([]byte("x")); err == nil {
		t.Fatal("Send must error when no route is active")
	}
}

func TestEngineFailsOverToAnotherCarrier(t *testing.T) {
	eng, _ := newEngineForTest()
	eng.SetProbeTiming(80*time.Millisecond, 240*time.Millisecond)

	ca := NewSimCarrier("yandex", 30*time.Millisecond)
	cb := NewSimCarrier("max", 30*time.Millisecond)
	ra := eng.Add(RouteDNA{Transport: "yandex", Exit: "tr"}, ca)
	eng.Add(RouteDNA{Transport: "max", Exit: "tr"}, cb)
	ra.SetState(StateActive)

	if err := eng.Start(); err != nil {
		t.Fatal(err)
	}
	defer eng.Stop()

	time.Sleep(500 * time.Millisecond) // establish healthy measurements on both
	if got := eng.brain.ActiveID(); got != ra.DNA.ID() {
		t.Fatalf("yandex should still be active, got %q", got)
	}

	// The active carrier goes fully down.
	ca.SetDown(true)

	deadline := time.Now().Add(3 * time.Second)
	var active string
	for time.Now().Before(deadline) {
		active = eng.brain.ActiveID()
		if active != ra.DNA.ID() {
			break
		}
		time.Sleep(60 * time.Millisecond)
	}
	if active == ra.DNA.ID() {
		t.Fatal("engine did not fail over after the active carrier went down")
	}
	// Traffic now flows through the surviving carrier.
	if err := eng.Send([]byte("x")); err != nil {
		t.Fatalf("Send after failover should succeed, got %v", err)
	}
}

func TestSimCarrierEchoMeasuresRealRTT(t *testing.T) {
	sim := NewSimCarrier("yandex", 40*time.Millisecond)
	_ = sim.Start()
	s := NewSampler()
	probe := NewMetricsProbe(sim, s)
	sim.SetInbound(func(raw []byte) { probe.Inbound(raw) })

	if err := probe.Ping(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(120 * time.Millisecond) // let the echo come back
	rtt := s.Snapshot().RTT
	if rtt <= 0 || rtt > 120*time.Millisecond {
		t.Fatalf("expected a measured RTT around 40ms, got %v", rtt)
	}
	_ = sim.Stop()
}
