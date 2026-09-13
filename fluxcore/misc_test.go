package fluxcore

import (
	"testing"
	"time"
)

func TestWeatherHoldsWithLittleHistory(t *testing.T) {
	f := WeatherFrom(90, []float64{90})
	if f.Now != Excellent || f.Plus60 != Excellent {
		t.Fatalf("with little history, forecast should hold: %+v", f)
	}
}

func TestWeatherProjectsDownwardTrend(t *testing.T) {
	hist := []float64{95, 88, 80, 72, 64}
	f := WeatherFrom(64, hist)
	if f.Now == Unstable {
		t.Fatal("current 64 should not already be unstable")
	}
	// A steady fall should make the +60 outlook worse than now.
	if !worse(f.Plus60, f.Now) {
		t.Fatalf("downward trend should worsen the outlook: %+v", f)
	}
}

func worse(a, b Condition) bool { return rank(a) > rank(b) }
func rank(c Condition) int {
	switch c {
	case Excellent:
		return 0
	case Good:
		return 1
	case Fair:
		return 2
	case Degrading:
		return 3
	default:
		return 4
	}
}

func TestBusReplayAndSubscribe(t *testing.T) {
	bus := NewBus(5)
	bus.Publish(Event{Kind: EvRouteDiscovered, Message: "one"})
	if got := bus.Recent(); len(got) != 1 || got[0].Message != "one" {
		t.Fatalf("replay buffer wrong: %+v", got)
	}
	ch, cancel := bus.Subscribe(4)
	defer cancel()
	bus.Publish(Event{Kind: EvFailoverStart, Message: "two"})
	select {
	case e := <-ch:
		if e.Message != "two" {
			t.Fatalf("subscriber got wrong event: %+v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber did not receive event")
	}
}

func TestBusReplayBounded(t *testing.T) {
	bus := NewBus(3)
	for i := 0; i < 10; i++ {
		bus.Publish(Event{Kind: EvInfo, Message: "x"})
	}
	if got := len(bus.Recent()); got != 3 {
		t.Fatalf("replay should be bounded to 3, got %d", got)
	}
}

func TestConfigDurationJSONRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/cfg.json"
	c := DefaultConfig()
	c.PingEvery = Duration(4 * time.Second)
	c.Routes = []RouteConfig{{Transport: "yandex", URL: "https://example"}}
	if err := c.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.PingEvery.D() != 4*time.Second {
		t.Fatalf("duration did not round-trip, got %v", got.PingEvery.D())
	}
	if len(got.Routes) != 1 || got.Routes[0].Transport != "yandex" {
		t.Fatalf("routes did not round-trip: %+v", got.Routes)
	}
}

func TestHumanVoice(t *testing.T) {
	if Human(EvFailoverStart) == string(EvFailoverStart) {
		t.Fatal("failover should have a human-friendly message")
	}
}
