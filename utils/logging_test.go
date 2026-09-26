package utils

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

// capture sets the level and collects what Packetf and Debugf print.
func capture(t *testing.T, lvl int) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	SetOutput(&buf)
	SetLevel(lvl)
	t.Cleanup(func() {
		SetLevel(LevelOff)
		SetOutput(nopWriter{})
	})
	return &buf
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestLevels(t *testing.T) {
	cases := []struct {
		level                  int
		packets, debug, hexdmp bool
	}{
		{LevelOff, false, false, false},
		{LevelPackets, true, false, false},
		{LevelDebug, true, true, false},
		{LevelHexdump, true, true, true},
	}
	for _, c := range cases {
		buf := capture(t, c.level)
		Packetf("packet-line")
		Debugf("debug-line")
		out := buf.String()
		if got := strings.Contains(out, "packet-line"); got != c.packets {
			t.Errorf("level %d: packet line printed=%v, want %v", c.level, got, c.packets)
		}
		if got := strings.Contains(out, "debug-line"); got != c.debug {
			t.Errorf("level %d: debug line printed=%v, want %v", c.level, got, c.debug)
		}
		if got := IsVerbose(); got != c.hexdmp {
			t.Errorf("level %d: IsVerbose=%v, want %v", c.level, got, c.hexdmp)
		}
	}
}

func TestSetLevelClamps(t *testing.T) {
	capture(t, 7)
	if Level() != LevelHexdump {
		t.Fatalf("SetLevel(7) -> %d, want %d", Level(), LevelHexdump)
	}
	SetLevel(-1)
	if Level() != LevelOff {
		t.Fatalf("SetLevel(-1) -> %d, want %d", Level(), LevelOff)
	}
}

func TestDebugShimsMeanOperationalLogs(t *testing.T) {
	capture(t, LevelOff)
	EnableDebug()
	if Level() != LevelDebug {
		t.Fatalf("EnableDebug -> level %d, want %d", Level(), LevelDebug)
	}
	SetDebug(false)
	if Level() != LevelOff {
		t.Fatalf("SetDebug(false) -> level %d, want %d", Level(), LevelOff)
	}
}

func TestLogSinkGetsPacketAndDebugLines(t *testing.T) {
	capture(t, LevelDebug)
	var mu sync.Mutex
	var got []string
	SetLogSink(func(s string) {
		mu.Lock()
		got = append(got, s)
		mu.Unlock()
	})
	defer SetLogSink(nil)
	Packetf("p %d", 1)
	Debugf("d %d", 2)
	if len(got) != 2 || got[0] != "p 1" || got[1] != "d 2" {
		t.Fatalf("sink got %q", got)
	}
}

// The mobile bridges flip the level and the output on every connect while
// the previous connection may still be logging; run with -race.
func TestConcurrentReconfigure(t *testing.T) {
	capture(t, LevelDebug)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					Debugf("x")
					Packetf("y")
				}
			}
		}()
	}
	for i := 0; i < 200; i++ {
		SetLevel(i % 4)
		SetOutput(nopWriter{})
		SetDebug(i%2 == 0)
	}
	close(stop)
	wg.Wait()
}
