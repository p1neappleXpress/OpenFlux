package phpbox

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/p1neappleXpress/OpenFlux/utils"
)

// captureLog runs fn at the given debug level and returns what was logged.
func captureLog(t *testing.T, level int, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	utils.SetOutput(&buf)
	utils.SetLevel(level)
	t.Cleanup(func() { utils.SetLevel(0) })
	fn()
	return buf.String()
}

// The stream mode obeys the same levels as the packet modes: nothing at 0,
// one line per frame at -d, operational lines at -dd, payload hexdumps at -ddd.
func TestStreamModeLogLevels(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); buf := make([]byte, 1024); n, _ := c.Read(buf); c.Write(buf[:n]) }()
		}
	}()
	host, portStr, _ := net.SplitHostPort(echo.Addr().String())
	port := 0
	for _, ch := range portStr {
		port = port*10 + int(ch-'0')
	}

	run := func() {
		pc := newPipeCarrier()
		m := NewMux(pc)
		_ = m.Start()
		defer m.Close()
		c, err := m.Dial(context.Background(), host, port)
		if err != nil {
			t.Error(err)
			return
		}
		c.Write([]byte("hello-stream"))
		buf := make([]byte, 64)
		c.Read(buf)
		c.Close()
		time.Sleep(50 * time.Millisecond)
	}

	if out := captureLog(t, 0, run); strings.Contains(out, "[STREAM]") {
		t.Errorf("level 0 must log nothing from the stream mode, got:\n%s", out)
	}
	d := captureLog(t, 1, run)
	for _, want := range []string{"[STREAM] -> ", "[STREAM] <- ", "stream 1 OPEN " + host, "OPEN_OK", "stream 1 DATA", "stream 1 CLOSE"} {
		if !strings.Contains(d, want) {
			t.Errorf("-d is missing %q in:\n%s", want, d)
		}
	}
	if strings.Contains(d, "dialing") || strings.Contains(d, "hexdump") {
		t.Errorf("-d must be packet lines only (no operational logs, no hexdump):\n%s", d)
	}
	dd := captureLog(t, 2, run)
	if !strings.Contains(dd, "dialing "+host) || !strings.Contains(dd, "closed by the app") || !strings.Contains(dd, "[STREAM] -> ") {
		t.Errorf("-dd must add operational logs to the packet lines:\n%s", dd)
	}
	if strings.Contains(dd, "hexdump") {
		t.Errorf("-dd must not hexdump:\n%s", dd)
	}
	ddd := captureLog(t, 3, run)
	if !strings.Contains(ddd, "hexdump") || !strings.Contains(ddd, "hello-stream") && !strings.Contains(ddd, "68 65 6c 6c 6f") {
		t.Errorf("-ddd must hexdump the DATA payload:\n%s", ddd)
	}
}

// busyCarrier pushes back ("write queue full") for the first n sends, like the
// Mail.ru transport under load, then accepts.
type busyCarrier struct {
	pipeCarrier
	busy   atomic.Int32
	sent   atomic.Int32
	failed atomic.Int32
}

func (b *busyCarrier) Send(p []byte) error {
	if b.busy.Add(-1) >= 0 {
		b.failed.Add(1)
		return errors.New("write queue full")
	}
	b.sent.Add(1)
	return b.pipeCarrier.Send(p)
}

// A frame must never be dropped because the carrier was busy: a silently lost
// DATA frame inside a TLS record is what broke handshakes under load.
func TestSendWaitsForABusyCarrier(t *testing.T) {
	echo, _ := net.Listen("tcp", "127.0.0.1:0")
	defer echo.Close()
	var got sync.WaitGroup
	got.Add(1)
	var received []byte
	go func() {
		c, err := echo.Accept()
		if err != nil {
			got.Done()
			return
		}
		defer c.Close()
		buf := make([]byte, 64)
		n, _ := c.Read(buf)
		received = append(received, buf[:n]...)
		got.Done()
		c.Write(buf[:n])
	}()
	host, portStr, _ := net.SplitHostPort(echo.Addr().String())
	port := 0
	for _, ch := range portStr {
		port = port*10 + int(ch-'0')
	}

	bc := &busyCarrier{pipeCarrier: *newPipeCarrier()}
	bc.busy.Store(40) // the first 40 sends are refused
	m := NewMux(bc)
	_ = m.Start()
	defer m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := m.Dial(ctx, host, port)
	if err != nil {
		t.Fatalf("Dial under a busy carrier: %v", err)
	}
	if _, err := c.Write([]byte("PAYLOAD")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got.Wait()
	if string(received) != "PAYLOAD" {
		t.Fatalf("destination received %q, want PAYLOAD (a frame was dropped)", received)
	}
	if bc.failed.Load() < 40 {
		t.Fatalf("carrier refused only %d sends; the test did not exercise the busy path", bc.failed.Load())
	}
}

// A carrier that stays down must surface an error, not hang or lose data silently.
func TestSendGivesUpOnADeadCarrier(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out sendPatience")
	}
	bc := &busyCarrier{pipeCarrier: *newPipeCarrier()}
	bc.busy.Store(1 << 30)
	m := NewMux(bc)
	_ = m.Start()
	defer m.Close()
	start := time.Now()
	_, err := m.Dial(context.Background(), "127.0.0.1", 1)
	if err == nil {
		t.Fatal("Dial on a dead carrier succeeded")
	}
	if d := time.Since(start); d < sendPatience-time.Second || d > sendPatience+3*time.Second {
		t.Fatalf("gave up after %v, want about %v", d, sendPatience)
	}
}
