package transport

import (
	"sync/atomic"
	"testing"
)

// An exit that restarted (a fresh session with no client) answers a client that still
// believes in the old one with a hello at once, so the client takes it over in a round
// trip instead of after the 30 s of silence it needs to notice (and instead of never,
// when it has not learned yet that its exit answers keepalives).
func TestRestartedExitSaysHelloToAClientThatSpeaksOutOfTurn(t *testing.T) {
	client, exit, _, _ := linkedSessions(t, "direct")
	var got atomic.Int32
	exit.Receive(func([]byte) { got.Add(1) })
	startPair(t, client, exit)
	if err := client.Send(testIPv4(40, 6)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "data before the restart", func() bool { return got.Load() == 1 })

	// The exit process restarts: it has a new challenge and no client; the client does not know.
	exit.mu.Lock()
	exit.resetLocked()
	exit.mu.Unlock()
	if exit.IsConnected() {
		t.Fatal("the exit still reports a client right after its reset")
	}

	eventually(t, "the client to be taken over without waiting for silence", func() bool {
		_ = client.Send(testIPv4(40, 6))
		return exit.IsConnected() && client.IsConnected()
	})
	before := got.Load()
	eventually(t, "data to flow again", func() bool {
		_ = client.Send(testIPv4(40, 6))
		return got.Load() > before
	})
}
