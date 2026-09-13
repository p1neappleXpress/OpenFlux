package fluxcore

import "time"

// Metrics is the extended per-route telemetry that FluxBrain reasons over.
//
// The original OpenFlux TransportStats only knew bytes/packets/reconnects.
// These are the fields that give the brain *vision*: how fast, how lossy,
// how stable and how alive a route is right now.
type Metrics struct {
	// Signal quality
	RTT        time.Duration // smoothed round-trip time (from ping/pong frames)
	Jitter     time.Duration // smoothed variation of RTT
	LossRatio  float64       // 0..1, fraction of probe frames lost
	Throughput float64       // bytes/sec, smoothed

	// Liveness
	HandshakeMs int64     // time to establish the underlying carrier
	LastSeen    time.Time // last successful inbound
	Connected   bool

	// Volume / history (carried over from the original stats)
	BytesSent  uint64
	BytesRecv  uint64
	Reconnects uint64

	LastError string
}
