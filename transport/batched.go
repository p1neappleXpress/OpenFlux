package transport

import (
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
	batchQueueDepth      = 4096
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

	running       atomic.Bool
	done          chan struct{}
	stopOnce      sync.Once
	wg            sync.WaitGroup
	queuedBytes   atomic.Int64
	maxQueueBytes int64
	encode        func([][]byte) []byte
	lifecycleMu   sync.Mutex
	started       bool
	stopped       bool

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
	encode := encodeBatch
	linger, batchBytes, batchCount := envInt("OPENFLUX_BATCH_LINGER_MS", defaultLingerMs), envInt("OPENFLUX_BATCH_BYTES", defaultMaxBatchBytes), envInt("OPENFLUX_BATCH_COUNT", defaultMaxBatchCount)
	if maxBytes > 0 {
		encode = encodeMobileBatch
		linger, batchBytes, batchCount = defaultLingerMs, defaultMaxBatchBytes, defaultMaxBatchCount
	}
	return &BatchedTransport{
		encode:        encode,
		Transport:     inner,
		queue:         make(chan []byte, queueDepth),
		done:          make(chan struct{}),
		maxQueueBytes: maxBytes,
		lingerMs:      linger,
		maxBatchBytes: batchBytes,
		maxBatchCount: batchCount,
	}
}

func (b *BatchedTransport) Start() error {
	b.lifecycleMu.Lock()
	defer b.lifecycleMu.Unlock()
	if b.started || b.stopped {
		return fmt.Errorf("batch transport already started")
	}
	b.started = true
	b.running.Store(true)
	if err := b.Transport.Start(); err != nil {
		b.running.Store(false)
		return err
	}
	b.wg.Add(1)
	go b.flushLoop()
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

// Send copies the packet (the caller's buffer is reused by gVisor) and enqueues
// it for batching. A full queue drops the packet; the tunnel's TCP will
// retransmit, same as the old "write queue full" behavior.
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
		pkts, err := decodeBatch(data)
		if err != nil {
			utils.Debugf("[BATCH] decode error (%d bytes): %v", len(data), err)
			return
		}
		b.mu.RLock()
		cb := b.userCb
		b.mu.RUnlock()
		if cb == nil {
			return
		}
		for _, p := range pkts {
			cb(p)
		}
	})
}

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
					b.Transport.Send(b.encode(batch))
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
						b.Transport.Send(b.encode(batch))
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

		b.Transport.Send(b.encode(batch))
	}
}
