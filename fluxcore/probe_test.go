package fluxcore

import (
	"errors"
	"testing"
	"time"
)

func TestProbeDataRoundTrip(t *testing.T) {
	c := newClock()
	link := &loopLink{}
	p := newMetricsProbe(link, newSampler(c.now), c.now)

	payload := []byte("hello flux")
	if err := p.SendData(payload); err != nil {
		t.Fatal(err)
	}
	// The framed bytes should decode back to the same payload via Inbound.
	body, isData := p.Inbound(link.last())
	if !isData {
		t.Fatal("data frame should be reported as data")
	}
	if string(body) != string(payload) {
		t.Fatalf("payload mismatch: %q", body)
	}
}

func TestProbePingPongMeasuresRTT(t *testing.T) {
	c := newClock()
	link := &loopLink{}
	s := newSampler(c.now)
	p := newMetricsProbe(link, s, c.now)

	if err := p.Ping(); err != nil {
		t.Fatal(err)
	}
	pingFrame := link.last()

	// Simulate the pong coming back 120ms later.
	c.add(120 * time.Millisecond)
	// The peer would answer our ping with a pong; emulate by turning the ping
	// frame into a pong (same seq) and feeding it to Inbound.
	pong := append([]byte{framePong}, pingFrame[1:]...)
	if _, isData := p.Inbound(pong); isData {
		t.Fatal("pong must not be treated as data")
	}
	if rtt := s.Snapshot().RTT; rtt < 110*time.Millisecond || rtt > 130*time.Millisecond {
		t.Fatalf("expected RTT ~120ms, got %v", rtt)
	}
}

func TestProbeAutoRepliesToPing(t *testing.T) {
	c := newClock()
	link := &loopLink{}
	p := newMetricsProbe(link, newSampler(c.now), c.now)

	// Feed an inbound ping; the probe should emit a pong on the link.
	ping := []byte{framePing, 0, 0, 0, 9}
	p.Inbound(ping)
	reply := link.last()
	if reply == nil || reply[0] != framePong {
		t.Fatalf("probe should auto-reply with a pong, got %v", reply)
	}
	if reply[4] != 9 {
		t.Fatal("pong must echo the ping sequence")
	}
}

func TestProbeSendErrorCountsAsLoss(t *testing.T) {
	c := newClock()
	s := newSampler(c.now)
	p := newMetricsProbe(&errLink{err: errors.New("queue full")}, s, c.now)
	if err := p.SendData([]byte("x")); err == nil {
		t.Fatal("expected send error")
	}
	if s.Snapshot().LossRatio <= 0 {
		t.Fatal("a failed send must register as loss")
	}
}

func TestProbeTimeoutSweepMarksLoss(t *testing.T) {
	c := newClock()
	link := &loopLink{}
	s := newSampler(c.now)
	p := newMetricsProbe(link, s, c.now)

	_ = p.Ping()
	c.add(10 * time.Second) // long past any reasonable timeout
	p.SweepTimeouts(6 * time.Second)
	if s.Snapshot().LossRatio <= 0 {
		t.Fatal("an unanswered ping past timeout must count as loss")
	}
}
