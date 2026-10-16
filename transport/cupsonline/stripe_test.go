package cupsonline

import (
	"bytes"
	"encoding/binary"
	"sync/atomic"
	"testing"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport"
)

// startStripedPair is startPair on the striped wire: the exit creates the
// rooms and prints the striped list, the client joins with it.
func startStripedPair(t *testing.T, cfg CupsonlineConfig) (exit, client *peer) {
	t.Helper()
	t.Setenv("CUPS_STRIPE", "")
	exit = mustStart(t, "", false, cfg)
	list := exit.RoomList()
	if _, striped, err := transport.UnpackRooms(list); err != nil || !striped {
		t.Fatalf("exit printed %q (striped=%v, %v), want a striped list", list, striped, err)
	}
	client = mustStart(t, list, true, cfg)
	if !client.striped {
		t.Fatal("client given a striped list must stripe")
	}
	waitFor(t, 5*time.Second, "all channels up", func() bool {
		return allConnected(exit) && allConnected(client)
	})
	return exit, client
}

// seqFrame is frame i of a stream: its index up front, then filler of a size
// that varies with i (up to ~30 KB, more than one cups message).
func seqFrame(i int) []byte {
	n := 40 + (i*7919)%30000
	b := make([]byte, 4+n)
	binary.BigEndian.PutUint32(b, uint32(i))
	for j := 4; j < len(b); j++ {
		b[j] = byte(i + j)
	}
	return b
}

// expectInOrder reads n frames and checks they are seqFrame(0..n-1), in order.
func expectInOrder(t *testing.T, p *peer, n int, within time.Duration) {
	t.Helper()
	deadline := time.After(within)
	for i := 0; i < n; i++ {
		select {
		case got := <-p.got:
			if !bytes.Equal(got, seqFrame(i)) {
				idx := -1
				if len(got) >= 4 {
					idx = int(binary.BigEndian.Uint32(got))
				}
				t.Fatalf("frame %d: got frame %d (%d bytes)", i, idx, len(got))
			}
		case <-deadline:
			t.Fatalf("only %d of %d frames arrived", i, n)
		}
	}
}

// sendWindowed pushes frames 0..n-1 without outrunning the far side by more
// than a window, the way TCP above the tunnel would: the stripe drops at the
// head of its queue (CoDel) once frames wait there longer than its target,
// so the window stays below what the slowest setup here moves in that time.
// It gives up when the receiving side has.
func sendWindowed(t *testing.T, from *peer, n int, received *atomic.Int64, rxDone <-chan struct{}) {
	t.Helper()
	window := int64(192 << 10)
	if raceEnabled {
		// A runner under -race moves far less than 192 KB in CoDel's 150 ms
		// target, so the stripe's queue would drop frames the in-order
		// tests count on.
		window = 64 << 10
	}
	var sent int64
	for i := 0; i < n; i++ {
		f := seqFrame(i)
		for sent-received.Load() > window {
			select {
			case <-rxDone:
				return
			case <-time.After(time.Millisecond):
			}
		}
		if err := from.Send(f); err != nil {
			t.Fatal(err)
		}
		sent += int64(len(f))
	}
}

func TestStripedFramesArriveInOrderBothWays(t *testing.T) {
	newFakeCups(t)
	cfg := fastConfig()
	exit, client := startStripedPair(t, cfg)

	const n = 300
	for _, dir := range []struct {
		name     string
		from, to *peer
	}{{"up", client, exit}, {"down", exit, client}} {
		t.Run(dir.name, func(t *testing.T) {
			var got atomic.Int64
			done := make(chan struct{})
			go func() {
				defer close(done)
				expectInOrderCounting(t, dir.to, n, 20*time.Second, &got)
			}()
			sendWindowed(t, dir.from, n, &got, done)
			<-done
		})
	}
	if used := roomsUsed(client); used < 2 {
		t.Fatalf("frames used only %d room(s), expected them spread", used)
	}
}

func expectInOrderCounting(t *testing.T, p *peer, n int, within time.Duration, got *atomic.Int64) {
	if raceEnabled {
		within *= 4 // a runner under -race is 5-10x slower
	}
	deadline := time.After(within)
	for i := 0; i < n; i++ {
		select {
		case b := <-p.got:
			if !bytes.Equal(b, seqFrame(i)) {
				idx := -1
				if len(b) >= 4 {
					idx = int(binary.BigEndian.Uint32(b))
				}
				t.Errorf("frame %d: got frame %d (%d bytes)", i, idx, len(b))
				return
			}
			got.Add(int64(len(b)))
		case <-deadline:
			t.Errorf("only %d of %d frames arrived", i, n)
			return
		}
	}
}

// A room's socket drops mid-stream: what was in flight on it is resent on
// the others, and the far side still gets every frame once and in order.
func TestStripedStreamSurvivesDroppedSocket(t *testing.T) {
	f := newFakeCups(t)
	cfg := fastConfig()
	cfg.SendInterval = 2 * time.Millisecond
	exit, client := startStripedPair(t, cfg)

	const n = 400
	var got atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		expectInOrderCounting(t, exit, n, 30*time.Second, &got)
	}()
	go func() {
		time.Sleep(150 * time.Millisecond)
		f.dropConns(client.wss[0].roomUUID)
	}()
	sendWindowed(t, client, n, &got, done)
	<-done
}

// The server swallows some messages (a room near its limit): on the striped
// wire a message holds whole pieces, so a lost one costs only its pieces,
// which are resent, and the stream never falls out of step.
func TestStripedStreamSurvivesLostMessages(t *testing.T) {
	f := newFakeCups(t)
	f.locked(func() { f.dropEvery = 7 })
	cfg := fastConfig()
	cfg.SendInterval = time.Millisecond
	exit, client := startStripedPair(t, cfg)
	const n = 300
	for _, dir := range []struct {
		name     string
		from, to *peer
	}{{"up", client, exit}, {"down", exit, client}} {
		t.Run(dir.name, func(t *testing.T) {
			var got atomic.Int64
			done := make(chan struct{})
			go func() {
				defer close(done)
				expectInOrderCounting(t, dir.to, n, 30*time.Second, &got)
			}()
			sendWindowed(t, dir.from, n, &got, done)
			<-done
		})
	}
}

// The point of the stripe: every room has its own send clock, so three rooms
// carry about three times what one does.
func TestStripedRoomsAddUp(t *testing.T) {
	if raceEnabled {
		t.Skip("a throughput measurement: meaningless under the race detector")
	}
	rate := func(rooms int) float64 {
		newFakeCups(t)
		cfg := fastConfig()
		cfg.NumRooms = rooms
		// 8 KB / 8 ms = 1 MB/s a room: each room's own clock is the limit,
		// not the CPU (three rooms at 12 KB / 4 ms is ~9 MB/s of cursor JSON
		// in one process, which a CI runner can't carry).
		cfg.MaxMessageData = 8192
		cfg.SendInterval = 8 * time.Millisecond
		exit, client := startStripedPair(t, cfg)
		const n = 250 // ~3.7 MB
		var got atomic.Int64
		start := time.Now()
		done := make(chan struct{})
		go func() {
			// Counts what arrives; order and completeness are the other
			// tests' business (here a CoDel drop is allowed).
			defer close(done)
			deadline := time.After(60 * time.Second)
			for i := 0; i < n; i++ {
				select {
				case b := <-exit.got:
					got.Add(int64(len(b)))
					if int(binary.BigEndian.Uint32(b)) == n-1 {
						return
					}
				case <-deadline:
					t.Errorf("%d room(s): only %d bytes arrived", rooms, got.Load())
					return
				}
			}
		}()
		sendWindowed(t, client, n, &got, done)
		<-done
		r := float64(got.Load()) / time.Since(start).Seconds()
		t.Logf("%d room(s): %.2f MB/s", rooms, r/1e6)
		exit.Stop()
		client.Stop()
		return r
	}
	one := rate(1)
	three := rate(3)
	if three < 2*one {
		t.Fatalf("3 rooms gave %.2f MB/s against %.2f MB/s on one: the rooms do not add up", three/1e6, one/1e6)
	}
}

// An old list (bare array) keeps the old wire on both roles, so a new build
// still talks to exits and clients from before the stripe.
func TestOldRoomListKeepsOldWire(t *testing.T) {
	newFakeCups(t)
	exit, client := startPair(t, fastConfig())
	if exit.striped || client.striped {
		t.Fatalf("old list: exit striped=%v client striped=%v, want neither", exit.striped, client.striped)
	}
	if exit.stripe.Load() != nil || client.stripe.Load() != nil {
		t.Fatal("old list must not run a stripe")
	}
	deliver(t, client, exit, []byte("old-wire"), 2*time.Second)
}
