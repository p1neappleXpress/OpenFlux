package transport

import "time"

// DiagnosticsSnapshot answers the app's "connected but no internet" screen:
// is the handshake established, when did the peer last answer, and what does
// every carrier look like. It exists so a user can tell, within seconds, a
// dead node from a dead network from a silent data plane - states that all
// look alike as "connected, nothing loads". The IPC status carries the same
// picture; this is the in-process form.
type DiagnosticsSnapshot struct {
	// Established: the Session handshake completed and the session is not
	// stopped. False while carriers are up means the peer is not answering
	// the handshake at all.
	Established bool
	// Mode names what IPv4 would go out as now: "session", "classic" or ""
	// (nothing yet) - see Mode.
	Mode string
	// LastHeardMs is how long ago the peer was last heard on any carrier,
	// in milliseconds; -1 when it was never heard. A session can be
	// established and still be silent: the bounds that fire on this number
	// are the reconnect contract (docs/reconnect.md).
	LastHeardMs int64
	// Carriers in priority order.
	Carriers []CarrierDiagnostics
}

// CarrierDiagnostics is one carrier's state. The instructive case is
// Connected without Heard: the carrier's own connection is up on this side
// while nothing arrives from the peer on it - the classic silent data plane.
type CarrierDiagnostics struct {
	Name        string
	Connected   bool  // the carrier's own connection is up on this side
	Heard       bool  // the peer was heard on it within linkTimeout
	LastHeardMs int64 // ms since the peer was last heard here; -1 never
}

// Diagnostics takes the snapshot. Cheap enough for a once-a-second status
// loop: a couple of map reads under the session lock.
func (s *Session) Diagnostics() DiagnosticsSnapshot {
	s.mu.Lock()
	snap := DiagnosticsSnapshot{Established: s.ready && !s.stopped, LastHeardMs: -1}
	for _, name := range s.order {
		l := s.links[name]
		if l == nil {
			continue
		}
		c := CarrierDiagnostics{
			Name:        name,
			Connected:   s.connectedLocked(l),
			Heard:       s.heardLocked(l),
			LastHeardMs: heardAgoMs(l.lastHeard),
		}
		if c.LastHeardMs >= 0 && (snap.LastHeardMs < 0 || c.LastHeardMs < snap.LastHeardMs) {
			snap.LastHeardMs = c.LastHeardMs
		}
		snap.Carriers = append(snap.Carriers, c)
	}
	s.mu.Unlock()
	snap.Mode = s.Mode()
	return snap
}

func heardAgoMs(t time.Time) int64 {
	if t.IsZero() {
		return -1
	}
	return time.Since(t).Milliseconds()
}
