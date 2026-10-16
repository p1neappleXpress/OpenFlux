package transport

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/p1neappleXpress/OpenFlux/utils"
)

// Defaults for the coalescing layer. Tunable at runtime via env vars so the
// batch size can be matched to the channel's per-message limits without a
// rebuild (OPENFLUX_BATCH_BYTES / OPENFLUX_BATCH_COUNT / OPENFLUX_BATCH_LINGER_MS).
const (
	defaultMaxBatchBytes = 8192
	defaultMaxBatchCount = 64
	defaultLingerMs      = 5
	batchQueueDepth      = 256 // At most 16 MiB of queued packet data.
	defaultQueueKB       = 512 // queued data waits behind at most this much
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

	queue chan []byte

	// queuedBytes/budget bound the data waiting in queue by size, not only by
	// packet count: with large packets 256 of them is megabytes, which every
	// flow of the tunnel queues behind. space wakes a waiting Send.
	queuedBytes   atomic.Int64
	budget        int64
	space         chan struct{}
	lingerMs      int
	maxBatchBytes int
	maxBatchCount int

	running    atomic.Bool
	lifecycle  sync.Mutex
	stopOnce   sync.Once
	stopCh     chan struct{}
	sendErrors atomic.Uint64
	queueWaits atomic.Uint64
	gate       compressGate // flushLoop only: whether to try zstd on a batch

	// pooled is set when the inner transport is an EncryptedTransport: it seals
	// every frame into a record of its own and keeps nothing of it, so the frame
	// buffer goes back to the pool once it is sent; and the plaintext it hands
	// up is a fresh buffer the batch decoder may alias.
	pooled bool
	lastFlush  time.Time    // flushLoop only: when the last batch went out

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

// NewBatchedTransport wraps inner with the coalescing layer. An optional
// batchBytesHint (the first variadic value, when > 0) raises the default batch
// size for a carrier whose per-connection limit is on message COUNT, not bytes
// — see BatchSizeHinter. OPENFLUX_BATCH_BYTES still overrides it.
func NewBatchedTransport(inner Transport, batchBytesHint ...int) *BatchedTransport {
	defBatchBytes := defaultMaxBatchBytes
	if len(batchBytesHint) > 0 && batchBytesHint[0] > 0 {
		defBatchBytes = batchBytesHint[0]
	}
	_, enc := inner.(*EncryptedTransport)
	b := &BatchedTransport{
		Transport:     inner,
		pooled:        enc,
		queue:         make(chan []byte, batchQueueDepth),
		lingerMs:      envInt("OPENFLUX_BATCH_LINGER_MS", defaultLingerMs),
		maxBatchBytes: min(envInt("OPENFLUX_BATCH_BYTES", defBatchBytes), maxFrameBytes-65537),
		maxBatchCount: min(envInt("OPENFLUX_BATCH_COUNT", defaultMaxBatchCount), maxFrameRecords-1),
		stopCh:        make(chan struct{}),
		space:         make(chan struct{}, 1),
		budget:        int64(envInt("OPENFLUX_BATCH_QUEUE_KB", defaultQueueKB)) << 10,
	}
	utils.Debugf("[BATCH] created: lingerMs=%d maxBatchBytes=%d maxBatchCount=%d queueCap=%d",
		b.lingerMs, b.maxBatchBytes, b.maxBatchCount, batchQueueDepth)
	return b
}

// SetLinger sets how long a partial batch waits for more frames (ms), unless
// OPENFLUX_BATCH_LINGER_MS overrides it; call before Start.
func (b *BatchedTransport) SetLinger(ms int) {
	if os.Getenv("OPENFLUX_BATCH_LINGER_MS") == "" && ms > 0 {
		b.lingerMs = ms
	}
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
// for batching. When the finite queue is full, it waits for capacity so the
// caller applies backpressure instead of losing the packet.
func (b *BatchedTransport) Send(data []byte) error {
	p := make([]byte, len(data))
	copy(p, data)
	return b.SendOwned(p)
}

// SendOwned is Send for a caller that hands the buffer over: it is queued as
// it is and must not be touched afterwards.
func (b *BatchedTransport) SendOwned(p []byte) error {
	if !b.running.Load() {
		if utils.DebugEnabled() {
			utils.Debugf("[BATCH] Send: not running, dropping %d bytes", len(p))
		}
		return fmt.Errorf("batched transport is not running")
	}
	if len(p) > 65535 {
		if utils.DebugEnabled() {
			utils.Debugf("[BATCH] Send: packet too large %d", len(p))
		}
		return fmt.Errorf("packet too large for batch record: %d bytes", len(p))
	}
	if !b.reserve(int64(len(p))) {
		return fmt.Errorf("batched transport is stopped")
	}
	select {
	case <-b.stopCh:
		b.queuedBytes.Add(-int64(len(p)))
		return fmt.Errorf("batched transport is stopped")
	case b.queue <- p:
	default:
		b.queueWaits.Add(1)
		select {
		case <-b.stopCh:
			b.queuedBytes.Add(-int64(len(p)))
			return fmt.Errorf("batched transport is stopped")
		case b.queue <- p:
		}
	}
	if !b.running.Load() {
		return fmt.Errorf("batched transport is stopped")
	}
	if utils.DebugEnabled() {
		utils.Debugf("[BATCH] Send: enqueued %d bytes (queue %d/%d)",
			len(p), len(b.queue), cap(b.queue))
	}
	return nil
}

// reserve waits until size more bytes fit the queue budget (an empty queue
// always takes one packet), counting them as queued. It reports false when the
// transport stops meanwhile. The wait is what makes a flow feel backpressure.
func (b *BatchedTransport) reserve(size int64) bool {
	if b.budget <= 0 {
		return true
	}
	waited := false
	for {
		cur := b.queuedBytes.Load()
		if cur == 0 || cur+size <= b.budget {
			if b.queuedBytes.CompareAndSwap(cur, cur+size) {
				return true
			}
			continue
		}
		if !waited {
			waited = true
			b.queueWaits.Add(1)
		}
		select {
		case <-b.space:
		case <-b.stopCh:
			return false
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// took accounts for a packet leaving the queue.
func (b *BatchedTransport) took(p []byte) {
	b.queuedBytes.Add(-int64(len(p)))
	select {
	case b.space <- struct{}{}:
	default:
	}
}

func (b *BatchedTransport) Stats() TransportStats {
	stats := b.Transport.Stats()
	stats.QueueWaits += b.queueWaits.Load()
	stats.SendRetries += b.sendErrors.Load()
	return stats
}

func (b *BatchedTransport) Receive(callback func([]byte)) {
	utils.Debugf("[BATCH] Receive: callback installed")
	b.mu.Lock()
	b.userCb = callback
	b.mu.Unlock()

	// Frames here are plaintext and can carry control messages with cookie
	// jars, so their hexdumps need --sensitive as well as -ddd.
	b.Transport.Receive(func(data []byte) {
		if utils.DebugEnabled() {
			utils.Debugf("[BATCH] Recv: %d wire bytes", len(data))
		}
		if utils.IsVerbose() && utils.Sensitive() {
			utils.Debugf("[BATCH] Recv wire hexdump:\n%s", hex.Dump(data))
		}
		pkts, err := decodeBatchOpt(data, b.pooled)
		if err != nil {
			utils.Debugf("[BATCH] decode error (%d bytes): %v", len(data), err)
			hintCodecMismatch(data, false)
			if utils.IsVerbose() && utils.Sensitive() {
				utils.Debugf("[BATCH] bad frame hexdump:\n%s", hex.Dump(data))
			}
			return
		}
		if utils.DebugEnabled() {
			utils.Debugf("[BATCH] Recv: decoded %d packets", len(pkts))
		}
		b.mu.RLock()
		cb := b.userCb
		b.mu.RUnlock()
		if cb == nil {
			utils.Debugf("[BATCH] Recv: no callback, dropping %d packets", len(pkts))
			return
		}
		for i, p := range pkts {
			if utils.DebugEnabled() {
				utils.Debugf("[BATCH] Recv: delivering packet %d/%d size=%d", i+1, len(pkts), len(p))
			}
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
			b.took(first)
			if utils.DebugEnabled() {
				utils.Debugf("[BATCH] flushLoop: dequeued first packet size=%d (queue %d/%d)",
					len(first), len(b.queue), cap(b.queue))
			}
		}
		batch := [][]byte{first}
		size := 2 + len(first)

		// Phase 1: absorb everything already queued (burst coalescing). This
		// alone collapses a window's worth of segments into one message.
	drainNow:
		for size < b.maxBatchBytes && len(batch) < b.maxBatchCount {
			select {
			case p := <-b.queue:
				b.took(p)
				batch = append(batch, p)
				size += 2 + len(p)
			default:
				break drainNow
			}
		}
		if utils.DebugEnabled() {
			utils.Debugf("[BATCH] flushLoop: phase1 drained to %d packets, %d bytes", len(batch), size)
		}

		// Phase 2: brief linger to catch stragglers arriving just after the
		// burst. Negligible next to the channel RTT, but it fills batches
		// during steady bulk transfer.
		// Nothing else was waiting and the carrier has been quiet: a lone packet
		// after a pause (a SYN, a request, an ACK) goes out at once. Lingering
		// only pays while packets keep coming. Done for carriers whose messages
		// are cheap; one that is rate limited in messages keeps coalescing.
		idle := b.gate.cheapBytes && len(batch) == 1 &&
			time.Since(b.lastFlush) > time.Duration(b.lingerMs)*time.Millisecond
		if !idle && b.lingerMs > 0 && size < b.maxBatchBytes && len(batch) < b.maxBatchCount {
			timer := time.NewTimer(time.Duration(b.lingerMs) * time.Millisecond)
		linger:
			for size < b.maxBatchBytes && len(batch) < b.maxBatchCount {
				select {
				case <-b.stopCh:
					timer.Stop()
					utils.Debugf("[BATCH] flushLoop: stop during linger, exiting")
					return
				case p := <-b.queue:
					b.took(p)
					batch = append(batch, p)
					size += 2 + len(p)
				case <-timer.C:
					break linger
				}
			}
			timer.Stop()
			if utils.DebugEnabled() {
				utils.Debugf("[BATCH] flushLoop: phase2 linger done, %d packets, %d bytes",
					len(batch), size)
			}
		}

		encoded := encodeBatchGated(batch, &b.gate)
		b.lastFlush = time.Now()
		// The packets were handed over with SendOwned (or copied by Send): their
		// bytes are in the frame now.
		for i, p := range batch {
			putBuf(p)
			batch[i] = nil
		}
		if utils.DebugEnabled() {
			utils.Debugf("[BATCH] flushLoop: sending batch of %d packets (%d raw -> %d wire bytes)",
				len(batch), size, len(encoded))
		}
		if utils.IsVerbose() && utils.Sensitive() {
			utils.Debugf("[BATCH] flushLoop: batch hexdump:\n%s", hex.Dump(encoded))
		}
		for b.running.Load() {
			if err := b.Transport.Send(encoded); err == nil {
				if utils.DebugEnabled() {
					utils.Debugf("[BATCH] flushLoop: batch sent OK")
				}
				if b.pooled {
					putBuf(encoded)
				}
				break
			} else if errors.Is(err, ErrRecordTooLarge) {
				// No retry will ever fit it: drop the frame (the flows above resend).
				b.recordSendError(err)
				utils.Infof("[BATCH] a frame of %d bytes cannot be carried and is dropped: %v", len(encoded), err)
				break
			} else {
				b.recordSendError(err)
			}
			// Keep this batch ahead of later ones while the carrier is full or
			// reconnecting. A finite wait avoids spinning on a nonblocking Send.
			timer := time.NewTimer(10 * time.Millisecond)
			select {
			case <-b.stopCh:
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}
	utils.Debugf("[BATCH] flushLoop: exit (running=false)")
}
