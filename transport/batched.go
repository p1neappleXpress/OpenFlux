package transport

import (
	"encoding/hex"
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
//
// BatchedTransport speaks wire-format v2 only. Capability negotiation lives
// in NegotiatedTransport (transport/negotiated.go); the retired wire-v3
// prototype is no longer supported and fails startup if forced.
type BatchedTransport struct {
	Transport

	queue         chan []byte
	lingerMs      int
	maxBatchBytes int
	maxBatchCount int

	running    atomic.Bool
	lifecycle  sync.Mutex
	stopOnce   sync.Once
	stopCh     chan struct{}
	sendErrors atomic.Uint64

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
	b := &BatchedTransport{
		Transport:     inner,
		queue:         make(chan []byte, batchQueueDepth),
		lingerMs:      envInt("OPENFLUX_BATCH_LINGER_MS", defaultLingerMs),
		maxBatchBytes: min(envInt("OPENFLUX_BATCH_BYTES", defaultMaxBatchBytes), maxFrameBytes-65537),
		maxBatchCount: min(envInt("OPENFLUX_BATCH_COUNT", defaultMaxBatchCount), maxFrameRecords-1),
		stopCh:        make(chan struct{}),
	}
	utils.Debugf("[BATCH] created: lingerMs=%d maxBatchBytes=%d maxBatchCount=%d queueCap=%d",
		b.lingerMs, b.maxBatchBytes, b.maxBatchCount, batchQueueDepth)
	return b
}

func (b *BatchedTransport) Start() error {
	utils.Debugf("[BATCH] Start() called")
	b.lifecycle.Lock()
	defer b.lifecycle.Unlock()
	if os.Getenv("OPENFLUX_EXPERIMENTAL_WIRE_V3") == "1" {
		return fmt.Errorf("unauthenticated wire-v3 negotiation has been retired; unset OPENFLUX_EXPERIMENTAL_WIRE_V3 and use --negotiate with encryption on both peers")
	}
	select {
	case <-b.stopCh:
		return fmt.Errorf("batched transport is stopped")
	default:
	}
	if b.running.Load() {
		utils.Debugf("[BATCH] Start: already running")
		return nil
	}
	utils.Debugf("[BATCH] Start: starting inner transport")
	if err := b.Transport.Start(); err != nil {
		utils.Debugf("[BATCH] Start: inner Start failed: %v", err)
		return err
	}
	b.running.Store(true)
	go b.flushLoop()
	utils.Debugf("[BATCH] Start: OK")
	return nil
}

func (b *BatchedTransport) Stop() error {
	utils.Debugf("[BATCH] Stop() called")
	b.lifecycle.Lock()
	defer b.lifecycle.Unlock()
	select {
	case <-b.stopCh:
		return nil
	default:
	}
	b.running.Store(false)
	b.stopOnce.Do(func() { close(b.stopCh) })
	return b.Transport.Stop()
}

// Send copies the packet (the caller's buffer may be reused) and enqueues it
// for batching. A full queue returns an explicit error; TCP may retransmit,
// while UDP callers must treat it as datagram loss.
func (b *BatchedTransport) Send(data []byte) error {
	b.lifecycle.Lock()
	defer b.lifecycle.Unlock()
	if !b.running.Load() {
		utils.Debugf("[BATCH] Send: not running, dropping %d bytes", len(data))
		return fmt.Errorf("batched transport is not running")
	}
	if len(data) > 65535 {
		utils.Debugf("[BATCH] Send: packet too large %d", len(data))
		return fmt.Errorf("packet too large for batch record: %d bytes", len(data))
	}
	p := make([]byte, len(data))
	copy(p, data)
	select {
	case b.queue <- p:
		utils.Debugf("[BATCH] Send: enqueued %d bytes (queue %d/%d)",
			len(p), len(b.queue), cap(b.queue))
		return nil
	default:
		utils.Debugf("[BATCH] Send: QUEUE FULL, dropped %d bytes", len(p))
		return fmt.Errorf("batch queue full")
	}
}

func (b *BatchedTransport) Receive(callback func([]byte)) {
	utils.Debugf("[BATCH] Receive: callback installed")
	b.mu.Lock()
	b.userCb = callback
	b.mu.Unlock()

	// Frames here are plaintext and can carry control messages with cookie
	// jars, so their hexdumps need --sensitive as well as -ddd.
	b.Transport.Receive(func(data []byte) {
		utils.Debugf("[BATCH] Recv: %d wire bytes", len(data))
		if utils.IsVerbose() && utils.Sensitive() {
			utils.Debugf("[BATCH] Recv wire hexdump:\n%s", hex.Dump(data))
		}
		pkts, err := decodeBatch(data)
		if err != nil {
			utils.Debugf("[BATCH] decode error (%d bytes): %v", len(data), err)
			if utils.IsVerbose() && utils.Sensitive() {
				utils.Debugf("[BATCH] bad frame hexdump:\n%s", hex.Dump(data))
			}
			return
		}
		utils.Debugf("[BATCH] Recv: decoded %d packets", len(pkts))
		b.mu.RLock()
		cb := b.userCb
		b.mu.RUnlock()
		if cb == nil {
			utils.Debugf("[BATCH] Recv: no callback, dropping %d packets", len(pkts))
			return
		}
		for i, p := range pkts {
			utils.Debugf("[BATCH] Recv: delivering packet %d/%d size=%d", i+1, len(pkts), len(p))
			if utils.IsVerbose() && utils.Sensitive() {
				utils.Debugf("[BATCH] packet %d hexdump:\n%s", i+1, hex.Dump(p))
			}
			cb(p)
		}
	})
}

// SendErrors counts Transport.Send failures observed by the batching layer.
func (b *BatchedTransport) SendErrors() uint64 { return b.sendErrors.Load() }

func (b *BatchedTransport) recordSendError(err error) {
	b.sendErrors.Add(1)
	utils.Debugf("[BATCH] send error (total=%d): %v", b.sendErrors.Load(), err)
}

func (b *BatchedTransport) flushLoop() {
	utils.Debugf("[BATCH] flushLoop: started")
	for b.running.Load() {
		var first []byte
		select {
		case <-b.stopCh:
			utils.Debugf("[BATCH] flushLoop: stop signal, exiting")
			return
		case first = <-b.queue:
			utils.Debugf("[BATCH] flushLoop: dequeued first packet size=%d (queue %d/%d)",
				len(first), len(b.queue), cap(b.queue))
		}
		batch := [][]byte{first}
		size := 2 + len(first)

		// Phase 1: absorb everything already queued (burst coalescing). This
		// alone collapses a window's worth of segments into one message.
	drainNow:
		for size < b.maxBatchBytes && len(batch) < b.maxBatchCount {
			select {
			case p := <-b.queue:
				batch = append(batch, p)
				size += 2 + len(p)
			default:
				break drainNow
			}
		}
		utils.Debugf("[BATCH] flushLoop: phase1 drained to %d packets, %d bytes", len(batch), size)

		// Phase 2: brief linger to catch stragglers arriving just after the
		// burst. Negligible next to the channel RTT, but it fills batches
		// during steady bulk transfer.
		if b.lingerMs > 0 && size < b.maxBatchBytes && len(batch) < b.maxBatchCount {
			timer := time.NewTimer(time.Duration(b.lingerMs) * time.Millisecond)
		linger:
			for size < b.maxBatchBytes && len(batch) < b.maxBatchCount {
				select {
				case <-b.stopCh:
					timer.Stop()
					utils.Debugf("[BATCH] flushLoop: stop during linger, exiting")
					return
				case p := <-b.queue:
					batch = append(batch, p)
					size += 2 + len(p)
				case <-timer.C:
					break linger
				}
			}
			timer.Stop()
			utils.Debugf("[BATCH] flushLoop: phase2 linger done, %d packets, %d bytes",
				len(batch), size)
		}

		encoded := encodeBatch(batch)
		utils.Debugf("[BATCH] flushLoop: sending batch of %d packets (%d raw -> %d wire bytes)",
			len(batch), size, len(encoded))
		if utils.IsVerbose() && utils.Sensitive() {
			utils.Debugf("[BATCH] flushLoop: batch hexdump:\n%s", hex.Dump(encoded))
		}
		if err := b.Transport.Send(encoded); err != nil {
			b.recordSendError(err)
		} else {
			utils.Debugf("[BATCH] flushLoop: batch sent OK")
		}
	}
	utils.Debugf("[BATCH] flushLoop: exit (running=false)")
}
