package cupsonline

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLiveCups runs an exit node and a client through the real cups.online.
// It creates rooms there, so it only runs when asked to:
//
//	OPENFLUX_CUPS_LIVE=1 go test -run TestLiveCups -v -timeout 15m ./transport/cupsonline/
//
// OPENFLUX_CUPS_LIVE_IDLE sets how long the channels sit idle (default 2m,
// longer than WSReadTimeout on purpose).
func TestLiveCups(t *testing.T) {
	if os.Getenv("OPENFLUX_CUPS_LIVE") == "" {
		t.Skip("set OPENFLUX_CUPS_LIVE=1 to run against the real cups.online")
	}
	idle := 2 * time.Minute
	if v := os.Getenv("OPENFLUX_CUPS_LIVE_IDLE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			t.Fatal(err)
		}
		idle = d
	}
	cfg := DefaultCupsonlineConfig()
	cfg.StatsInterval = time.Hour

	start := time.Now()
	exit := mustStart(t, "", false, cfg)
	packed := packRooms(exit.RoomUUIDs())
	client := mustStart(t, packed, true, cfg)
	waitFor(t, 60*time.Second, "all channels up", func() bool {
		return allConnected(exit) && allConnected(client)
	})
	t.Logf("%d rooms created and joined on both sides in %v", len(exit.wss), time.Since(start).Round(time.Millisecond))

	t.Run("frames both ways", func(t *testing.T) {
		drain(exit)
		// Frames spread across rooms and may arrive in a different order, so
		// check they all arrive intact, not the order.
		var want [][]byte
		for i := 0; i < 50; i++ {
			f := frame(i)
			want = append(want, f)
			client.Send(f)
		}
		expectSet(t, exit, want)

		drain(exit)
		drain(client)
		var rtts []time.Duration
		for i := 0; i < 10; i++ {
			sent := time.Now()
			client.Send(frame(500 + i))
			next(t, exit)
			exit.Send(frame(600 + i))
			next(t, client)
			rtts = append(rtts, time.Since(sent))
		}
		t.Logf("round trip client->exit->client: %v", rtts)
	})

	t.Run("throughput", func(t *testing.T) {
		// Feed packets steadily rather than all at once; the transport paces
		// itself under that so the server doesn't drop the connection. Frames
		// spread across all rooms and can arrive in a different order, so this
		// checks that every one arrives intact, not the order.
		drain(exit)
		const n, size = 200, 8 << 10
		body := bytes.Repeat([]byte{0x5a}, size)
		seen := make([]bool, n)
		done := make(chan int, 1)
		go func() {
			count := 0
			for count < n {
				select {
				case p := <-exit.got:
					id := int(p[0])<<8 | int(p[1])
					if id < 0 || id >= n || seen[id] || len(p) != size+2 {
						done <- -1
						return
					}
					seen[id] = true
					count++
				case <-time.After(30 * time.Second):
					done <- count
					return
				}
			}
			done <- n
		}()
		sent := time.Now()
		for i := 0; i < n; i++ {
			p := append([]byte{byte(i >> 8), byte(i)}, body...)
			if err := client.Send(p); err != nil {
				t.Fatal(err)
			}
			time.Sleep(15 * time.Millisecond) // offered load; the transport paces the rest
		}
		if got := <-done; got != n {
			t.Fatalf("only %d of %d packets arrived intact", got, n)
		}
		took := time.Since(sent)
		t.Logf("client->exit: %d KB across %d rooms in %v = %.0f KB/s",
			n*size>>10, len(client.wss), took.Round(time.Millisecond), float64(n*size>>10)/took.Seconds())
	})

	t.Run("idle longer than the read timeout", func(t *testing.T) {
		reconnects := func() (n uint64) {
			for _, p := range []*peer{exit, client} {
				for _, ws := range p.wss {
					n += ws.stats.reconnects.Load()
				}
			}
			return
		}
		before := reconnects()
		t.Logf("sitting idle for %v (read timeout %v)", idle, cfg.WSReadTimeout)
		time.Sleep(idle)
		if n := reconnects() - before; n != 0 {
			t.Errorf("%d reconnects while idle: keepalive doesn't keep the read side busy", n)
		}
		deliver(t, client, exit, []byte("after idle"), 10*time.Second)
		deliver(t, exit, client, []byte("after idle back"), 10*time.Second)
	})

	t.Run("dropped socket comes back without a page reload", func(t *testing.T) {
		ws := client.wss[0]
		was := ws.auth()
		ws.writeMu.Lock()
		if ws.conn != nil {
			ws.conn.Close()
		}
		ws.writeMu.Unlock()

		deliver(t, client, exit, []byte("during reconnect"), 15*time.Second)
		waitFor(t, 30*time.Second, "dropped channel back", func() bool { return ws.connected.Load() })
		if ws.auth() != was {
			t.Error("reconnect after a plain drop re-joined the room (page reload)")
		}
	})

	t.Run("re-join keeps the session", func(t *testing.T) {
		a := client.wss[0].auth()
		again, err := joinRoom(context.Background(), a.roomUUID, a.httpClient)
		if err != nil {
			t.Fatal(err)
		}
		if again.userUUID != a.userUUID {
			t.Errorf("same session, new user: %s -> %s", a.userUUID, again.userUUID)
		}
	})

	t.Run("exit restart reuses its rooms", func(t *testing.T) {
		exit.Stop()
		again := mustStart(t, packed, false, cfg)
		if strings.Join(again.RoomUUIDs(), ",") != strings.Join(exit.RoomUUIDs(), ",") {
			t.Fatalf("restarted exit node is in other rooms: %v vs %v", again.RoomUUIDs(), exit.RoomUUIDs())
		}
		waitFor(t, 60*time.Second, "restarted exit node up", func() bool { return allConnected(again) })
		deliver(t, client, again, []byte("to restarted exit"), 15*time.Second)
		deliver(t, again, client, []byte("from restarted exit"), 15*time.Second)
	})
}

// TestLiveThroughputCalibrate finds how fast one channel may send before
// cups.online starts dropping the connection. It drives a real pair at a few
// SendInterval values and reports, per value, how much arrived and how many
// times the channel had to reconnect. Pick the fastest interval with no loss
// and no reconnects for DefaultCupsonlineConfig.SendInterval.
//
//	OPENFLUX_CUPS_LIVE=1 go test -run TestLiveThroughputCalibrate -v -timeout 15m ./transport/cupsonline/
func TestLiveThroughputCalibrate(t *testing.T) {
	if os.Getenv("OPENFLUX_CUPS_LIVE") == "" {
		t.Skip("set OPENFLUX_CUPS_LIVE=1 to run against the real cups.online")
	}
	cfg := DefaultCupsonlineConfig()
	cfg.StatsInterval = time.Hour

	exit := mustStart(t, "", false, cfg)
	// Measure one channel: the client joins only the first room, so the whole
	// offered load rides that single channel at the interval under test.
	oneRoom := packRooms(exit.RoomUUIDs()[:1])
	waitFor(t, 60*time.Second, "exit up", func() bool { return allConnected(exit) })

	const m, size = 120, 8 << 10
	body := bytes.Repeat([]byte{0x5a}, size)

	t.Logf("%-10s %-12s %-12s %-10s", "interval", "arrived", "reconnects", "KB/s")
	for _, iv := range []time.Duration{40, 25, 18, 12, 8, 5} {
		interval := iv * time.Millisecond
		pcfg := cfg
		pcfg.SendInterval = interval
		cl, err := startPeer(t, oneRoom, true, pcfg)
		if err != nil {
			t.Fatalf("interval %v: client start: %v", interval, err)
		}
		waitFor(t, 60*time.Second, "client up", func() bool { return allConnected(cl) })
		drain(exit) // clear the previous phase's leftovers
		reconBefore := cl.wss[0].stats.reconnects.Load()

		start := time.Now()
		for i := 0; i < m; i++ {
			if err := cl.Send(append([]byte{byte(i >> 8), byte(i)}, body...)); err != nil {
				break
			}
		}
		arrived := 0
		deadline := time.After(30 * time.Second)
	collect:
		for arrived < m {
			select {
			case <-exit.got:
				arrived++
			case <-deadline:
				break collect
			}
		}
		elapsed := time.Since(start)
		recon := cl.wss[0].stats.reconnects.Load() - reconBefore
		rate := float64(arrived*size>>10) / elapsed.Seconds()
		t.Logf("%-10v %-12s %-12d %-10.0f", interval, fmt.Sprintf("%d/%d", arrived, m), recon, rate)
		cl.Stop()
		time.Sleep(2 * time.Second) // let the server settle between phases
	}
	exit.Stop()
}
