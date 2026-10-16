package transport

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

type TransportConfig struct {
	MaxReconnectAttempts int
	ReconnectDelay       time.Duration
	ReconnectMultiplier  float64
	MaxQueueSize         int
	KeepAliveInterval    time.Duration
	// Network the carrier dials through (netbind): "" is the default
	// route, or netbind.NetworkCellular, NetworkWiFi, NetworkEthernet, so
	// a Session's carriers may leave through different networks.
	Network string
}

type Transport interface {
	Start() error
	Stop() error
	Send(data []byte) error
	Receive(callback func([]byte))
	IsConnected() bool
	Stats() TransportStats
}

type TransportStats struct {
	BytesSent     uint64
	BytesReceived uint64
	PacketsSent   uint64
	PacketsRecv   uint64
	Reconnects    uint64
	QueueWaits    uint64
	SendRetries   uint64
	Connected     bool
	Uptime        time.Duration
}

// BatchSizeHinter is an optional interface a transport implements to raise the
// default coalescing batch size for its carrier. A carrier capped by the number
// of messages per connection rather than by bytes wants each message as large
// as possible: the Mail.ru docs chat channel, for instance, is cut by the
// server's anti-abuse after ~12 messages regardless of their size, so a small
// batch wastes most of the per-connection budget. newLink passes the hint to
// NewBatchedTransport; OPENFLUX_BATCH_BYTES still overrides it.
type BatchSizeHinter interface {
	PreferredBatchBytes() int
}

// CheapBytesCarrier is an optional interface for a carrier whose bytes cost
// little next to the CPU spent compressing them (a plain TCP connection, as
// opposed to a document in somebody else's cloud that moves a few hundred
// KB/s). The batching layer then compresses only batches that shrink by a
// good margin, and at the fastest level.
type CheapBytesCarrier interface {
	CheapBytes() bool
}

// PreferredCarrier is an optional interface for a carrier that goes straight to
// the peer (a plain TCP connection to the exit) as opposed to one that goes
// through somebody else's service. A Session sends over such a carrier
// whenever it is live, whatever priority the profile gives it: the profiles
// rank the document carriers above direct, which was meant as the backup
// for when they are blocked, but a path that works is better than any relay,
// faster and without the relay's rate limits. When it stops reaching the
// peer the next carrier takes over, and takes the traffic back when it
// returns. It changes nothing on the wire and nothing the peer must agree on.
type PreferredCarrier interface {
	Preferred() bool
}

// preferredBoost lifts a PreferredCarrier above every priority a profile sets.
const preferredBoost = 1000

// DefaultCarrierMTU is the IP packet size a carrier is assumed to move without
// trouble when it says nothing: what a stack gets on an ordinary network.
const DefaultCarrierMTU = 1500

// CarrierMTUer is an optional interface for a carrier that moves large IP
// packets as easily as small ones (a TCP stream): the tunnel stack then runs
// a larger MTU, which divides the per-packet work of the whole pipeline.
// A Session reports the smallest value over its carriers.
type CarrierMTUer interface {
	CarrierMTU() int
}

// StreamCarrierer is an optional interface for a transport whose data goes over reliable,
// ordered byte streams (direct TCP) at the moment. The tunnel stack then runs the TCP
// that suits a path that never loses a packet: Reno, which does not leave slow start on
// the jitter of the carrier the way CUBIC's HyStart does (+50-70 % on a single download
// over a real path), instead of CUBIC, which suits carriers that drop packets.
type StreamCarrierer interface {
	OnReliableStream() bool
}

// RecordHeadroom is the room a HeadroomSender wants in front of a record.
const RecordHeadroom = 2

// HeadroomSender is an optional interface of a carrier that puts a small
// header in front of every record (DirectTransport: a 2-byte length). The
// caller builds the record with RecordHeadroom spare bytes in front, the
// carrier fills them in and keeps the buffer: one copy of the record less on
// the way to the socket. The carrier owns buf from the call on, also when it
// returns an error it is free to have dropped it.
type HeadroomSender interface {
	SendWithHeadroom(buf []byte) error
}

// FillSender is an optional interface of a transport that can have a packet
// written straight into the buffer it is going to send: the caller says how
// big the IPv4 packet is and fill copies it into dst. The tunnel stack uses it
// to skip the intermediate copy of every packet it emits.
type FillSender interface {
	SendFill(size int, fill func(dst []byte)) error
}

func DefaultConfig() TransportConfig {
	return TransportConfig{
		MaxReconnectAttempts: 999999,
		ReconnectDelay:       0,
		ReconnectMultiplier:  1.1,
		MaxQueueSize:         1024,
		KeepAliveInterval:    10 * time.Second,
	}
}

type BaseTransport struct {
	config    TransportConfig
	running   atomic.Int32
	connected atomic.Int32
	stats     TransportStats
	startTime time.Time

	receiveCallback func([]byte)
	Mu              sync.RWMutex

	reconnectAttempts atomic.Int32

	// done is closed by Stop. Subclasses can select on Done() to
	// interrupt sleeps and backoff loops.
	done     chan struct{}
	doneOnce sync.Once
}

func NewBaseTransport(config TransportConfig) *BaseTransport {
	return &BaseTransport{
		config:    config,
		startTime: time.Now(),
		done:      make(chan struct{}),
	}
}

func (b *BaseTransport) Start() error {
	b.doneOnce = sync.Once{}
	b.done = make(chan struct{})
	b.running.Store(1)
	b.startTime = time.Now()
	return nil
}

func (b *BaseTransport) Stop() error {
	b.running.Store(0)
	b.connected.Store(0)
	if b.done != nil {
		b.doneOnce.Do(func() { close(b.done) })
	}
	return nil
}

// Done returns a channel that closes when the transport is stopped.
// Subclasses and callers can select on it to interrupt sleeps.
func (b *BaseTransport) Done() <-chan struct{} {
	b.Mu.RLock()
	d := b.done
	b.Mu.RUnlock()
	if d == nil {
		// Never started; return a channel that blocks forever.
		return make(chan struct{})
	}
	return d
}

func (b *BaseTransport) IsRunning() bool {
	return b.running.Load() == 1
}

func (b *BaseTransport) IsConnected() bool {
	return b.connected.Load() == 1
}

func (b *BaseTransport) SetConnected(connected bool) {
	if connected {
		b.connected.Store(1)
	} else {
		b.connected.Store(0)
	}
}

func (b *BaseTransport) Receive(callback func([]byte)) {
	b.Mu.Lock()
	defer b.Mu.Unlock()
	b.receiveCallback = callback
}

func (b *BaseTransport) CallReceive(data []byte) {
	b.Mu.RLock()
	cb := b.receiveCallback
	b.Mu.RUnlock()
	if cb != nil {
		cb(data)
	}
}

func (b *BaseTransport) GetSession(accessor func(interface{})) {
	b.Mu.RLock()
	defer b.Mu.RUnlock()
	// This is a helper for subclasses
}

func (b *BaseTransport) Stats() TransportStats {
	return TransportStats{
		BytesSent:     atomic.LoadUint64(&b.stats.BytesSent),
		BytesReceived: atomic.LoadUint64(&b.stats.BytesReceived),
		PacketsSent:   atomic.LoadUint64(&b.stats.PacketsSent),
		PacketsRecv:   atomic.LoadUint64(&b.stats.PacketsRecv),
		Reconnects:    uint64(b.reconnectAttempts.Load()),
		Connected:     b.IsConnected(),
		Uptime:        time.Since(b.startTime),
	}
}

func (b *BaseTransport) RecordSend(bytes int) {
	atomic.AddUint64(&b.stats.BytesSent, uint64(bytes))
	atomic.AddUint64(&b.stats.PacketsSent, 1)
}

func (b *BaseTransport) RecordReceive(bytes int) {
	atomic.AddUint64(&b.stats.BytesReceived, uint64(bytes))
	atomic.AddUint64(&b.stats.PacketsRecv, 1)
}

func (b *BaseTransport) RecordReconnect() {
	b.reconnectAttempts.Add(1)
}

func (b *BaseTransport) GetConfig() TransportConfig {
	return b.config
}

// PacketTooLargeError is what Send answers for an IPv4 packet that no live
// carrier of the session can move: Limit is the largest size one can. The
// tunnel stack turns it into an ICMP "fragmentation needed", after which the
// connection carries on with smaller segments.
type PacketTooLargeError struct{ Limit int }

func (e *PacketTooLargeError) Error() string {
	return fmt.Sprintf("session: packet exceeds what the live carriers move (%d bytes)", e.Limit)
}
