package transport

import "testing"

// The diagnostics behind the app's "connected but no internet" screen: the
// established handshake, the freshest last-heard clock and every carrier's
// row - where Connected without Heard tells a silent data plane from a dead
// network.
func TestSessionDiagnostics(t *testing.T) {
	client, exit, cw, ew := linkedSessions(t, "direct", "yandex")
	fastKeepalive(client, exit)
	startPair(t, client, exit)

	d := client.Diagnostics()
	if !d.Established || d.Mode != "session" {
		t.Fatalf("diagnostics = %+v, want an established session", d)
	}
	if len(d.Carriers) != 2 {
		t.Fatalf("carriers = %+v, want both", d.Carriers)
	}
	for _, c := range d.Carriers {
		if !c.Connected || !c.Heard || c.LastHeardMs < 0 {
			t.Fatalf("carrier %q = %+v, want connected and heard", c.Name, c)
		}
	}
	if d.LastHeardMs < 0 || d.LastHeardMs > int64(timeLinkTimeoutMs) {
		t.Fatalf("last heard = %dms, want fresh", d.LastHeardMs)
	}

	// Both carriers stop reaching the peer: still connected on this side,
	// nothing heard - the silent data plane, visible as such within a
	// linkTimeout.
	blackhole(cw["direct"], ew["direct"], cw["yandex"], ew["yandex"])
	eventually(t, "both carriers to age out of heard", func() bool {
		for _, c := range client.Diagnostics().Carriers {
			if c.Heard {
				return false
			}
		}
		return true
	})
	d = client.Diagnostics()
	if !d.Established {
		t.Fatal("session reset before its silence bound")
	}
	for _, c := range d.Carriers {
		if !c.Connected {
			t.Fatalf("carrier %q lost Connected with only the wire blackholed", c.Name)
		}
		if c.Heard || c.LastHeardMs < int64(timeLinkTimeoutMs) {
			t.Fatalf("carrier %q = %+v, want silent and stale", c.Name, c)
		}
	}
	if d.LastHeardMs < int64(timeLinkTimeoutMs) {
		t.Fatalf("last heard = %dms, want stale", d.LastHeardMs)
	}
}

// timeLinkTimeoutMs mirrors fastKeepalive's linkTimeout for the bound
// checks above.
const timeLinkTimeoutMs = 100
