package transport

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"openflux/utils"
)

// Defaults for the coalescing layer. Tunable at runtime via env vars so the
// batch size can be matched to the channel's per-message limits without a
// rebuild (OPENFLUX_BATCH_BYTES / OPENFLUX_BATCH_COUNT / OPENFLUX_BATCH_LINGER_MS).
const (
	defaultMaxBatchBytes = 8192
	defaultMaxBatchCount = 64
	defaultLingerMs      = 5
	batchQueueDepth      = 256 // At most 16 MiB of queued packet data.
)

// BatchedTransport replaces the old per-packet CompressedTransport. It queues
// outgoing tunnel packets, coalesces bursts into a single framed+zstd batch per
// inner transport message, and splits batches back into packets on receive.
//
// This is the symmetric layer: client and exit node must both use it (they do,
// because main.go wraps both the same way).
type BatchedTransport struct {
	Transport

	queue         chan []byte
	lingerMs      int
	maxBatchBytes int
	maxBatchCount int

	running         atomic.Bool
	done            chan struct{}
	stopOnce        sync.Once
	wg              sync.WaitGroup
	queuedBytes     atomic.Int64
	maxQueueBytes   int64
	encode          func([][]byte) []byte
	lifecycleMu     sync.Mutex
	started         bool
	stopped         bool
	experimentalV3  bool
	sendErrors      atomic.Uint64
	peerCaps        atomic.Uint32
	peerSeen        atomic.Bool
	peerAck         atomic.Bool
	sessionID       uint32
	sequence        atomic.Uint64
	peerSession     atomic.Uint32
	peerSequence    atomic.Uint64
	reorderedFrames atomic.Uint64

	mu     sync.RWMutex
	userCb func([]byte)
}

func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func NewBatchedTransport(inner Transport) *BatchedTransport {
	return NewBatchedTransportWithLimits(inner, batchQueueDepth, 0)
}

// NewBatchedTransportWithLimits changes only buffering, never the wire format.
// A zero byte limit retains desktop behavior.
func NewBatchedTransportWithLimits(inner Transport, queueDepth int, maxBytes int64) *BatchedTransport {
	var sessionBytes [4]byte
	_, _ = rand.Read(sessionBytes[:])
	sessionID := binary.BigEndian.Uint32(sessionBytes[:])
	if sessionID == 0 {
		sessionID = 1
	}
	encode := encodeBatch
	linger, batchBytes, batchCount := envInt("OPENFLUX_BATCH_LINGER_MS", defaultLingerMs), envInt("OPENFLUX_BATCH_BYTES", defaultMaxBatchBytes), envInt("OPENFLUX_BATCH_COUNT", defaultMaxBatchCount)
	if maxBytes > 0 {
		encode = encodeMobileBatch
		linger, batchBytes, batchCount = defaultLingerMs, defaultMaxBatchBytes, defaultMaxBatchCount
	}
	return &BatchedTransport{
		experimentalV3: os.Getenv("OPENFLUX_EXPERIMENTAL_WIRE_V3") == "1" && maxBytes == 0,
		sessionID:      sessionID,
		encode:         encode,
		Transport:      inner,
		queue:          make(chan []byte, queueDepth),
		done:           make(chan struct{}),
		maxQueueBytes:  maxBytes,
		lingerMs:       linger,
		maxBatchBytes:  min(batchBytes, maxFrameBytes-65537),
		maxBatchCount:  min(batchCount, maxFrameRecords-1),
	}
}

func (b *BatchedTransport) Start() error {
	b.lifecycleMu.Lock()
	defer b.lifecycleMu.Unlock()
	if b.stopped {
		return fmt.Errorf("batch transport stopped")
	}
	if b.started {
		return nil
	}
	if err := b.Transport.Start(); err != nil {
		return err
	}
	b.started = true
	b.running.Store(true)
	b.wg.Add(1)
	go b.flushLoop()
	if b.experimentalV3 {
		b.wg.Add(1)
		go b.capabilityLoop()
	}
	return nil
}

func (b *BatchedTransport) Stop() error {
	b.lifecycleMu.Lock()
	defer b.lifecycleMu.Unlock()
	if b.stopped {
		return nil
	}
	b.stopped = true
	b.running.Store(false)
	b.stopOnce.Do(func() { close(b.done) })
	err := b.Transport.Stop()
	b.wg.Wait()
	return err
}

// Send copies the packet (the caller's buffer may be reused) and enqueues it
// for batching. A full queue returns an explicit error; TCP may retransmit,
// while UDP callers must treat it as datagram loss.
func (b *BatchedTransport) Send(data []byte) error {
	if !b.running.Load() {
		return fmt.Errorf("batch transport stopped")
	}
	if len(data) == 0 || len(data) > 65535 {
		return fmt.Errorf("invalid batch packet size")
	}
	n := int64(len(data))
	if used := b.queuedBytes.Add(n); b.maxQueueBytes > 0 && used > b.maxQueueBytes {
		b.queuedBytes.Add(-n)
		return fmt.Errorf("batch queue byte limit")
	}
	p := make([]byte, len(data))
	copy(p, data)
	select {
	case b.queue <- p:
		return nil
	default:
		b.queuedBytes.Add(-n)
		return fmt.Errorf("batch queue full")
	}
}

func (b *BatchedTransport) Receive(callback func([]byte)) {
	b.mu.Lock()
	b.userCb = callback
	b.mu.Unlock()

	b.Transport.Receive(func(data []byte) {
		pkts, metadata, err := decodeBatchFrame(data)
		if err != nil {
			utils.Debugf("[BATCH] decode error (%d bytes): %v", len(data), err)
			return
		}
		if metadata.version == wireFormatVersion {
			if !b.experimentalV3 {
				return
			}
			b.observeWireFrame(metadata)
		}
		filtered := pkts[:0]
		for _, p := range pkts {
			caps, ack, ok := decodeCapabilityRecord(p)
			if ok {
				if !b.experimentalV3 {
					continue
				}
				b.peerCaps.Store(uint32(caps))
				b.peerSeen.Store(true)
				if ack {
					b.peerAck.Store(true)
				}
				continue
			}
			filtered = append(filtered, p)
		}
		b.mu.RLock()
		cb := b.userCb
		b.mu.RUnlock()
		if cb == nil {
			return
		}
		for _, p := range filtered {
			cb(p)
		}
	})
}

// PeerCapabilities reports capabilities advertised by a v3-aware peer. A
// false second result means the peer is legacy or negotiation has not finished.
func (b *BatchedTransport) PeerCapabilities() (Capabilities, bool) {
	return Capabilities(b.peerCaps.Load()), b.peerSeen.Load()
}

func (b *BatchedTransport) observeWireFrame(metadata wireMetadata) {
	previousSession := b.peerSession.Swap(metadata.sessionID)
	if previousSession != metadata.sessionID {
		b.peerSequence.Store(metadata.sequence)
		return
	}
	previous := b.peerSequence.Swap(metadata.sequence)
	if metadata.sequence <= previous {
		b.reorderedFrames.Add(1)
	}
}

func (b *BatchedTransport) ReorderedFrames() uint64 { return b.reorderedFrames.Load() }

func (b *BatchedTransport) capabilityLoop() {
	defer b.wg.Done()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-b.done:
			return
		case <-ticker.C:
		}
		ack := b.peerSeen.Load()
		if err := b.Transport.Send(encodeBatch([][]byte{encodeCapabilityRecord(DefaultCapabilities, ack)})); err != nil {
			b.recordSendError(err)
		}
		if ack && b.peerAck.Load() {
			return
		}
	}
}

func (b *BatchedTransport) sendBatch(batch [][]byte) {
	if b.experimentalV3 && !b.peerAck.Load() {
		withHello := make([][]byte, 0, len(batch)+1)
		withHello = append(withHello, encodeCapabilityRecord(DefaultCapabilities, b.peerSeen.Load()))
		batch = append(withHello, batch...)
	}
	var wire []byte
	if b.experimentalV3 && b.peerSeen.Load() && Capabilities(b.peerCaps.Load())&CapabilityWireV3 != 0 {
		wire = encodeBatchV3(batch, b.sessionID, b.sequence.Add(1))
	} else {
		wire = b.encode(batch)
	}
	if err := b.Transport.Send(wire); err != nil {
		b.recordSendError(err)
	}
}

func (b *BatchedTransport) recordSendError(err error) {
	b.sendErrors.Add(1)
	utils.Debugf("[BATCH] send error: %v", err)
}

func (b *BatchedTransport) SendErrors() uint64 { return b.sendErrors.Load() }

func (b *BatchedTransport) flushLoop() {
	defer b.wg.Done()
	for b.running.Load() {
		var first []byte
		select {
		case <-b.done:
			return
		case first = <-b.queue:
			b.queuedBytes.Add(-int64(len(first)))
		}
		batch := [][]byte{first}
		size := 2 + len(first)

		// Phase 1: absorb everything already queued (burst coalescing). This
		// alone collapses a window's worth of segments into one message.
	drainNow:
		for size < b.maxBatchBytes && len(batch) < b.maxBatchCount {
			select {
			case <-b.done:
				return
			case p, ok := <-b.queue:
				b.queuedBytes.Add(-int64(len(p)))
				if !ok {
					b.sendBatch(batch)
					return
				}
				batch = append(batch, p)
				size += 2 + len(p)
			default:
				break drainNow
			}
		}

		// Phase 2: brief linger to catch stragglers arriving just after the
		// burst. Negligible next to the channel RTT, but it fills batches
		// during steady bulk transfer.
		if b.lingerMs > 0 && size < b.maxBatchBytes && len(batch) < b.maxBatchCount {
			timer := time.NewTimer(time.Duration(b.lingerMs) * time.Millisecond)
		linger:
			for size < b.maxBatchBytes && len(batch) < b.maxBatchCount {
				select {
				case <-b.done:
					timer.Stop()
					return
				case p, ok := <-b.queue:
					b.queuedBytes.Add(-int64(len(p)))
					if !ok {
						timer.Stop()
						b.sendBatch(batch)
						return
					}
					batch = append(batch, p)
					size += 2 + len(p)
				case <-timer.C:
					break linger
				}
			}
			timer.Stop()
		}

		b.sendBatch(batch)
	}
}
