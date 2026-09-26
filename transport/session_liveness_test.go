package transport

import (
	"sync/atomic"
	"testing"
	"time"
)

func fastKeepalive(sessions ...*Session) {
	for _, s := range sessions {
		s.keepaliveInterval = 20 * time.Millisecond
		s.linkTimeout = 100 * time.Millisecond
	}
}

func blackhole(wires ...*startCountingWire) {
	for _, w := range wires {
		w.mu.Lock()
		w.drop = true
		w.mu.Unlock()
	}
}

func sentCount(w *startCountingWire) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.packets)
}

func startPair(t *testing.T, client, exit *Session) {
	t.Helper()
	if err := exit.Start(); err != nil {
		t.Fatal(err)
	}
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "both sides ready", func() bool { return client.IsConnected() && exit.IsConnected() })
}

// A carrier can stay attached to its document (IsConnected) while the
// peer's side of it is down; traffic must move to a carrier that still
// reaches the peer.
func TestSessionFailsOverWhenCarrierStopsReachingPeer(t *testing.T) {
	client, exit, cw, ew := linkedSessions(t, "direct", "yandex")
	fastKeepalive(client, exit)
	var got atomic.Int32
	exit.Receive(func([]byte) { got.Add(1) })
	startPair(t, client, exit)
	eventually(t, "keepalive support to be detected", func() bool {
		client.mu.Lock()
		defer client.mu.Unlock()
		return client.peerKeepalive
	})

	if got := client.ActiveTransport(); got != "direct" {
		t.Fatalf("active transport = %q, want direct", got)
	}
	blackhole(cw["direct"], ew["direct"])
	eventually(t, "data to arrive over the remaining carrier", func() bool {
		_ = client.Send(testIPv4(40, 6))
		return got.Load() > 0
	})
	if !client.IsConnected() {
		t.Fatal("session reported down while a carrier still reaches the peer")
	}
	eventually(t, "the active transport to follow the failover", func() bool {
		return client.ActiveTransport() == "yandex"
	})
}

func TestSessionPrefersHigherPriorityCarrier(t *testing.T) {
	client, exit, cw, _ := linkedSessions(t, "direct", "yandex")
	var got atomic.Int32
	exit.Receive(func([]byte) { got.Add(1) })
	startPair(t, client, exit)

	before := sentCount(cw["yandex"])
	for i := 0; i < 5; i++ {
		if err := client.Send(testIPv4(40, 6)); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "data delivery", func() bool { return got.Load() == 5 })
	if after := sentCount(cw["yandex"]); after != before {
		t.Fatalf("lower-priority carrier carried data while the preferred one was live (%d -> %d)", before, after)
	}
}

// Peers that predate keepalive never answer pings; their carriers must not
// age out of routing.
func TestSessionToleratesPeerWithoutKeepalive(t *testing.T) {
	client, exit, _, _ := linkedSessions(t, "direct")
	fastKeepalive(client, exit)
	exit.noPong = true
	var got atomic.Int32
	exit.Receive(func([]byte) { got.Add(1) })
	startPair(t, client, exit)

	time.Sleep(300 * time.Millisecond)
	if !client.IsConnected() {
		t.Fatal("carrier aged out against a peer without keepalive")
	}
	if err := client.Send(testIPv4(40, 6)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "data delivery", func() bool { return got.Load() == 1 })
}
