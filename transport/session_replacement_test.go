package transport

import (
	"crypto/rand"
	"sync/atomic"
	"testing"

	"openflux/transport/control"
)

// restartOn builds a fresh Session (a restarted process) on an existing
// wire of the given side.
func restartOn(t *testing.T, exit bool, wire *startCountingWire) *Session {
	t.Helper()
	s, err := NewSession(testParams, exit)
	if err != nil {
		t.Fatal(err)
	}
	fastKeepalive(s)
	if err := s.AddTransport("direct", wire, testSessionSecret, testSessionCtx, 100); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Stop() })
	return s
}

func TestSessionAcceptsRestartedClient(t *testing.T) {
	client, exit, cw, _ := linkedSessions(t, "direct")
	var got atomic.Int32
	exit.Receive(func([]byte) { got.Add(1) })
	startPair(t, client, exit)
	_ = client.Stop()

	restarted := restartOn(t, false, cw["direct"])
	if err := restarted.Start(); err != nil {
		t.Fatalf("exit did not accept a restarted client: %v", err)
	}
	eventually(t, "data from the restarted client", func() bool {
		_ = restarted.Send(testIPv4(40, 6))
		return got.Load() > 0
	})
}

func TestSessionRehandshakesAfterExitRestart(t *testing.T) {
	client, exit, _, ew := linkedSessions(t, "direct")
	fastKeepalive(client, exit)
	startPair(t, client, exit)
	eventually(t, "keepalive support to be detected", func() bool {
		client.mu.Lock()
		defer client.mu.Unlock()
		return client.peerKeepalive
	})
	_ = exit.Stop()

	restarted := restartOn(t, true, ew["direct"])
	var got atomic.Int32
	restarted.Receive(func([]byte) { got.Add(1) })
	if err := restarted.Start(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the client to reach the restarted exit", func() bool {
		_ = client.Send(testIPv4(40, 6))
		return got.Load() > 0
	})
}

func hello(t *testing.T, local, peer [32]byte) []byte {
	t.Helper()
	raw, err := (&control.Envelope{
		Kind:  control.KindHello,
		Role:  control.RoleClient,
		Local: local,
		Peer:  peer,
		Hello: &control.HelloTail{
			Capabilities:  control.Capabilities(testParams.Capabilities),
			MaxPacketSize: uint16(testParams.MaxPacketSize),
		},
	}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// Old hellos replayed from a carrier must not displace the established
// peer: only an echo of a challenge minted for the new sender does.
func TestSessionIgnoresReplayedHellos(t *testing.T) {
	client, exit, _, _ := linkedSessions(t, "direct")
	var got atomic.Int32
	exit.Receive(func([]byte) { got.Add(1) })
	startPair(t, client, exit)

	exit.mu.Lock()
	peer, current := exit.peer, exit.local
	exit.mu.Unlock()
	var stranger, random [32]byte
	_, _ = rand.Read(stranger[:])
	_, _ = rand.Read(random[:])
	link := exit.links["direct"]

	for _, replay := range [][]byte{
		hello(t, stranger, [32]byte{}), // initial hello
		hello(t, stranger, current),    // echo of the exit's standing challenge
		hello(t, stranger, random),     // echo of anything else
	} {
		exit.receive(link, replay)
		exit.mu.Lock()
		unchanged := exit.ready && exit.peer == peer
		exit.mu.Unlock()
		if !unchanged {
			t.Fatal("replayed hello displaced the established peer")
		}
	}
	if err := client.Send(testIPv4(40, 6)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "data from the established client", func() bool { return got.Load() == 1 })

	exit.mu.Lock()
	fresh := exit.candidate.local
	exit.mu.Unlock()
	exit.receive(link, hello(t, stranger, fresh))
	exit.mu.Lock()
	replaced := exit.peer == stranger && exit.local == fresh
	exit.mu.Unlock()
	if !replaced {
		t.Fatal("an echo of the fresh challenge did not replace the peer")
	}
}
