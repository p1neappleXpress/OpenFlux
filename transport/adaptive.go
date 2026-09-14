package transport

import (
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"universal-bypass-tool/utils"
)

// Tunables for the coalescing layer, overridable at runtime so batch size can
// be matched to the channel's per-message limits without a rebuild.
const (
	defaultMaxBatchBytes = 8192
	defaultMaxBatchCount = 64
	defaultLingerMs      = 5
	batchQueueDepth      = 4096
	probeInterval        = 3 * time.Second
)

func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// AdaptiveTransport is a self-negotiating codec over an inner transport. It
// lets a new (batching) build talk to an old (per-packet) peer without either
// side breaking, and upgrades to batching automatically once both sides
// support it — no handshake, no user setting.
//
// The two wire frame families never collide on their first byte:
//   - legacy : 0x00 (raw) or 0x1F (LZ4)  — one packet per frame (compressor.go)
//   - batch  : 0x02                        — many packets per zstd frame (framing.go)
//
// A peer on the OLD codec cannot decode a batch frame, but it drops it
// harmlessly: the bytes reach gVisor, which sees a non-IPv4 version nibble and
// discards them. That makes an empty batch frame safe to use as a capability
// probe.
//
// Negotiation:
//   - Receive accepts BOTH families (dispatch on the first byte).
//   - Send starts in LEGACY (compatible with any peer).
//   - Each side periodically emits a tiny empty-batch probe; a legacy peer
//     drops it, a batch-capable peer recognises it.
//   - The moment a side RECEIVES any batch frame it flips its own Send to batch.
//
// Outcome — new<->new upgrades and runs fast; new<->old keeps working in
// legacy; nobody breaks when only one side updates.
//
// Self-echo safety: this relies on the transport NOT echoing a sender its own
// frames (the same property that already lets the plain tunnel work — data
// frames would otherwise be re-injected). So a received batch frame always came
// from the peer, never from us.
type AdaptiveTransport struct {
	Transport // inner; IsConnected/Stats delegate to it

	queue     chan []byte
	peerBatch atomic.Bool
	running   atomic.Bool

	lingerMs      int
	maxBatchBytes int
	maxBatchCount int

	mu     sync.RWMutex
	userCb func([]byte)
}

func NewAdaptiveTransport(inner Transport) *AdaptiveTransport {
	return &AdaptiveTransport{
		Transport:     inner,
		queue:         make(chan []byte, batchQueueDepth),
		lingerMs:      envInt("OPENFLUX_BATCH_LINGER_MS", defaultLingerMs),
		maxBatchBytes: envInt("OPENFLUX_BATCH_BYTES", defaultMaxBatchBytes),
		maxBatchCount: envInt("OPENFLUX_BATCH_COUNT", defaultMaxBatchCount),
	}
}

func (a *AdaptiveTransport) Start() error {
	if err := a.Transport.Start(); err != nil {
		return err
	}
	a.running.Store(true)
	go a.flushLoop()
	go a.probeLoop()
	return nil
}

func (a *AdaptiveTransport) Stop() error {
	a.running.Store(false)
	return a.Transport.Stop()
}

// Send copies the packet (gVisor reuses the caller's buffer) and enqueues it. A
// full queue drops the packet; the tunneled TCP retransmits.
func (a *AdaptiveTransport) Send(data []byte) error {
	p := make([]byte, len(data))
	copy(p, data)
	select {
	case a.queue <- p:
		return nil
	default:
		return fmt.Errorf("adaptive queue full")
	}
}

func (a *AdaptiveTransport) Receive(callback func([]byte)) {
	a.mu.Lock()
	a.userCb = callback
	a.mu.Unlock()

	a.Transport.Receive(func(data []byte) {
		if len(data) > 0 && data[0] == batchFormatVersion {
			// A batch frame proves the peer speaks batch — upgrade our sends.
			a.peerBatch.Store(true)
			pkts, err := decodeBatch(data)
			if err != nil {
				utils.Debugf("[ADAPT] batch decode error (%d bytes): %v", len(data), err)
				return
			}
			a.deliver(pkts)
			return
		}
		// Legacy family (0x00 raw / 0x1F LZ4). decompress falls back to the raw
		// bytes on error, matching the old CompressedTransport behavior.
		pkt, err := decompress(data)
		if err != nil {
			a.deliver([][]byte{data})
			return
		}
		a.deliver([][]byte{pkt})
	})
}

func (a *AdaptiveTransport) deliver(pkts [][]byte) {
	a.mu.RLock()
	cb := a.userCb
	a.mu.RUnlock()
	if cb == nil {
		return
	}
	for _, p := range pkts {
		if len(p) > 0 {
			cb(p)
		}
	}
}

// probeLoop advertises batch capability. The empty-batch frame is 2 bytes; a
// legacy peer discards it, a batch peer flips to sending us batches. We keep
// probing (slowly) even after negotiation so a peer that reconnects re-learns.
func (a *AdaptiveTransport) probeLoop() {
	probe := encodeBatch(nil) // [0x02, 0x00]
	for a.running.Load() {
		time.Sleep(probeInterval)
		if !a.running.Load() {
			return
		}
		if a.IsConnected() {
			_ = a.Transport.Send(probe)
		}
	}
}

func (a *AdaptiveTransport) flushLoop() {
	for a.running.Load() {
		first, ok := <-a.queue
		if !ok {
			return
		}

		// Legacy mode: the peer hasn't proven batch support yet. Send this
		// packet on its own, compressed exactly like the old codec so any old
		// peer decodes it.
		if !a.peerBatch.Load() {
			_ = a.Transport.Send(compress(first))
			continue
		}

		// Batch mode: coalesce the queued burst (+ a short linger) into one
		// frame — the throughput win, since the channel is message-bound.
		batch := [][]byte{first}
		size := 2 + len(first)
	drainNow:
		for size < a.maxBatchBytes && len(batch) < a.maxBatchCount {
			select {
			case p, ok := <-a.queue:
				if !ok {
					_ = a.Transport.Send(encodeBatch(batch))
					return
				}
				batch = append(batch, p)
				size += 2 + len(p)
			default:
				break drainNow
			}
		}
		if a.lingerMs > 0 && size < a.maxBatchBytes && len(batch) < a.maxBatchCount {
			timer := time.NewTimer(time.Duration(a.lingerMs) * time.Millisecond)
		linger:
			for size < a.maxBatchBytes && len(batch) < a.maxBatchCount {
				select {
				case p, ok := <-a.queue:
					if !ok {
						timer.Stop()
						_ = a.Transport.Send(encodeBatch(batch))
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
		_ = a.Transport.Send(encodeBatch(batch))
	}
}
