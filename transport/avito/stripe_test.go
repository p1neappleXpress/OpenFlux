package avito

import (
	"bytes"
	"encoding/binary"
	"sync/atomic"
	"testing"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport"
)

func fastConfig() AvitoConfig {
	c := DefaultAvitoConfig()
	c.NumRooms = 3
	c.RoomCreatePause = time.Millisecond
	c.ReconnectMinDelay = 20 * time.Millisecond
	c.ReconnectMaxDelay = 200 * time.Millisecond
	c.WSHandshakeTimeout = 2 * time.Second
	c.StatsInterval = time.Hour
	return c
}

type peer struct {
	*AvitoTransport
	got chan []byte
}

func mustStart(t *testing.T, url string, isClient bool, cfg AvitoConfig) *peer {
	t.Helper()
	tr := NewAvitoTransport(url, transport.DefaultConfig(), "", isClient)
	tr.config = cfg
	p := &peer{AvitoTransport: tr, got: make(chan []byte, 8192)}
	tr.Receive(func(b []byte) { p.got <- append([]byte(nil), b...) })
	t.Cleanup(func() { tr.Stop() })
	if err := tr.Start(); err != nil {
		t.Fatalf("start (client=%v): %v", isClient, err)
	}
	return p
}

func allConnected(p *peer) bool {
	for _, ws := range p.allWS() {
		if !ws.connected.Load() {
			return false
		}
	}
	return true
}

func waitFor(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func startStripedPair(t *testing.T, cfg AvitoConfig) (exit, client *peer) {
	t.Helper()
	exit = mustStart(t, "", false, cfg)
	list := exit.RoomList()
	if _, striped, err := transport.UnpackRooms(list); err != nil || !striped {
		t.Fatalf("exit printed %q (striped=%v, %v), want a striped list", list, striped, err)
	}
	client = mustStart(t, list, true, cfg)
	if !client.striped {
		t.Fatal("client given a striped list must stripe")
	}
	waitFor(t, 5*time.Second, "all rooms up", func() bool {
		return allConnected(exit) && allConnected(client)
	})
	return exit, client
}

// seqFrame is frame i of a stream: its index, then filler of a size that
// varies with i (up to ~60 KB, more than one stripe piece).
func seqFrame(i int) []byte {
	n := 40 + (i*7919)%60000
	b := make([]byte, 4+n)
	binary.BigEndian.PutUint32(b, uint32(i))
	for j := 4; j < len(b); j++ {
		b[j] = byte(i + j)
	}
	return b
}

func expectInOrder(t *testing.T, p *peer, n int, within time.Duration, got *atomic.Int64) {
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

// sendWindowed keeps at most window bytes unanswered, like TCP above the
// tunnel; it gives up when the receiving side has.
func sendWindowed(t *testing.T, from *peer, n, window int, received *atomic.Int64, rxDone <-chan struct{}) {
	t.Helper()
	var sent int64
	for i := 0; i < n; i++ {
		f := seqFrame(i)
		for sent-received.Load() > int64(window) {
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
	newFakeAvito(t, 5*time.Millisecond)
	exit, client := startStripedPair(t, fastConfig())
	const n = 200
	for _, dir := range []struct {
		name     string
		from, to *peer
	}{{"up", client, exit}, {"down", exit, client}} {
		t.Run(dir.name, func(t *testing.T) {
			var got atomic.Int64
			done := make(chan struct{})
			go func() { defer close(done); expectInOrder(t, dir.to, n, 30*time.Second, &got) }()
			sendWindowed(t, dir.from, n, 512<<10, &got, done)
			<-done
		})
	}
}

// A room's sockets drop mid-stream: the op in flight is lost with them, the
// stripe resends its pieces on the other pairs, and nothing is lost or
// reordered.
func TestStripedStreamSurvivesDroppedSocket(t *testing.T) {
	f := newFakeAvito(t, 5*time.Millisecond)
	exit, client := startStripedPair(t, fastConfig())
	const n = 300
	var got atomic.Int64
	done := make(chan struct{})
	go func() { defer close(done); expectInOrder(t, exit, n, 30*time.Second, &got) }()
	go func() {
		time.Sleep(100 * time.Millisecond)
		f.dropConns(client.txWss[0].roomID)
	}()
	sendWindowed(t, client, n, 512<<10, &got, done)
	<-done
}

// One op at a time per room, each taking opDelay: three pairs carry about
// three times what one does.
func TestStripedPairsAddUp(t *testing.T) {
	if raceEnabled {
		t.Skip("a throughput measurement: meaningless under the race detector")
	}
	rate := func(pairs int) float64 {
		newFakeAvito(t, 40*time.Millisecond)
		cfg := fastConfig()
		cfg.NumRooms = pairs
		cfg.ChunkBytes = 64 << 10 // 64 KB / 40 ms = 1.6 MB/s a pair
		exit, client := startStripedPair(t, cfg)
		const n = 200 // ~6 MB
		var got atomic.Int64
		start := time.Now()
		done := make(chan struct{})
		go func() {
			defer close(done)
			deadline := time.After(60 * time.Second)
			for {
				select {
				case b := <-exit.got:
					got.Add(int64(len(b)))
					if int(binary.BigEndian.Uint32(b)) == n-1 {
						return
					}
				case <-deadline:
					t.Errorf("%d pair(s): only %d bytes arrived", pairs, got.Load())
					return
				}
			}
		}()
		// Under what one pair moves in CoDel's 150 ms target, per pair: a
		// window shared by all pairs caps three at what it lets through.
		sendWindowed(t, client, n, pairs*192<<10, &got, done)
		<-done
		r := float64(got.Load()) / time.Since(start).Seconds()
		t.Logf("%d pair(s): %.2f MB/s", pairs, r/1e6)
		exit.Stop()
		client.Stop()
		return r
	}
	one := rate(1)
	three := rate(3)
	if three < 2*one {
		t.Fatalf("3 pairs gave %.2f MB/s against %.2f MB/s on one: the pairs do not add up", three/1e6, one/1e6)
	}
}

// An old list (bare array, 0.7.0) keeps the old wire.
func TestOldRoomListKeepsOldWire(t *testing.T) {
	newFakeAvito(t, time.Millisecond)
	t.Setenv("AVITO_STRIPE", "0")
	cfg := fastConfig()
	exit := mustStart(t, "", false, cfg)
	if exit.striped {
		t.Fatal("AVITO_STRIPE=0: the exit must create an old list")
	}
	client := mustStart(t, exit.RoomList(), true, cfg)
	if client.striped || client.stripe.Load() != nil {
		t.Fatal("old list: the client must not stripe")
	}
	waitFor(t, 5*time.Second, "all rooms up", func() bool { return allConnected(exit) && allConnected(client) })
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		client.Send([]byte("old-wire"))
		select {
		case b := <-exit.got:
			if string(b) == "old-wire" {
				return
			}
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatal("nothing crossed on the old wire")
}
