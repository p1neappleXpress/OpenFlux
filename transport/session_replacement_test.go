package transport

import (
	"crypto/rand"
	"sync/atomic"
	"testing"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport/control"
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

// The exit died before the client's first pong arrived, so the fast reset is
// not armed. The client must still give up on the silence on its own and
// rehandshake with the restarted exit, not zombie until restarted.
func TestSessionRecoversWhenKeepaliveWasNeverNegotiated(t *testing.T) {
	client, exit, _, ew := linkedSessions(t, "direct")
	fastKeepalive(client, exit)
	exit.noPong = true
	startPair(t, client, exit)
	client.mu.Lock()
	armed := client.peerKeepalive
	client.mu.Unlock()
	if armed {
		t.Fatal("keepalive support detected without a pong")
	}
	_ = exit.Stop()

	restarted := restartOn(t, true, ew["direct"])
	var got atomic.Int32
	restarted.Receive(func([]byte) { got.Add(1) })
	if err := restarted.Start(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the keepalive-less client to rehandshake with the restarted exit", func() bool {
		_ = client.Send(testIPv4(40, 6))
		return got.Load() > 0
	})
}

// The client pings for its first pong as soon as the handshake completes,
// not on the next keepalive tick: a node restart in the session's first
// seconds must find the client's reset armed.
func TestSessionPingsRightAfterHandshake(t *testing.T) {
	client, exit, _, _ := linkedSessions(t, "direct")
	for _, s := range []*Session{client, exit} {
		s.keepaliveInterval = time.Hour // no tick within the test
		s.linkTimeout = 100 * time.Millisecond
	}
	startPair(t, client, exit)
	eventually(t, "keepalive support to be detected without a tick", func() bool {
		client.mu.Lock()
		defer client.mu.Unlock()
		return client.peerKeepalive
	})
}

// A lost first reply must not mute the exit: an initial hello is answered
// every time it is offered while the session is open.
func TestSessionAnswersRepeatedInitialHello(t *testing.T) {
	_, exit, _, ew := linkedSessions(t, "direct")
	if err := exit.Start(); err != nil {
		t.Fatal(err)
	}
	var stranger [32]byte
	_, _ = rand.Read(stranger[:])
	link := exit.links["direct"]

	for i := 1; i <= 3; i++ {
		exit.receive(link, hello(t, stranger, [32]byte{}))
		want := i
		eventually(t, "a reply to the initial hello", func() bool {
			return sentCount(ew["direct"]) >= want
		})
	}
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
