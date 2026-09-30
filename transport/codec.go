package transport

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/p1neappleXpress/OpenFlux/utils"
)

// Classic-mode codec names, as --codec and openflux:// links spell them.
const (
	CodecBatched = "batched"
	CodecLegacy  = "legacy"
)

// Codec fallback timing for the side that speaks first (the client): with
// nothing heard from the peer, the other framing is tried codecFirstFallback
// after the first send, then the two alternate every codecFallbackEvery.
// That is faster than the KDF context rotation (see EncryptedTransport), so
// both framings get tried under every candidate context.
const (
	codecFirstFallback = 2 * time.Second
	codecFallbackEvery = time.Second
)

// CodecTransport is the classic (non-Session) mode's codec. It replaces the
// fixed BatchedTransport / CompressedTransport pair, which had to match on
// both peers: batched against legacy dropped every packet in silence.
//
// The two framings never collide on their first byte:
//
//	batch-v2  0x02         many packets per frame, zstd when it helps
//	legacy    0x00 / 0x1F  one packet per frame, raw / LZ4
//
// so every frame is decoded whichever framing it uses, and the sending
// framing follows the peer:
//
//   - once the peer has sent a batch frame (an empty one is the probe the
//     iOS fork's adaptive codec sends), frames go out batched;
//   - once it has sent only legacy frames, they go out legacy;
//   - before anything is heard, the preferred framing is used; the
//     initiating side (the client) switches to the other one when the peer
//     stays silent, so a peer that decodes only one framing still gets
//     through.
//
// Old batched-only or legacy-only peers therefore keep working, and
// updated peers converge on batching.
type CodecTransport struct {
	Transport

	preferred bool // true: batched
	initiator bool

	batchNow   atomic.Bool // current send framing before the peer is known
	peerBatch  atomic.Bool
	peerLegacy atomic.Bool
	heard      atomic.Bool
	firstSend  atomic.Int64
	lastSwitch atomic.Int64

	queue         chan []byte
	lingerMs      int
	maxBatchBytes int
	maxBatchCount int

	running   atomic.Bool
	lifecycle sync.Mutex
	stopOnce  sync.Once
	stopCh    chan struct{}

	mu     sync.RWMutex
	userCb func([]byte)

	sendErrors atomic.Uint64
	unknown    atomic.Uint64
}

// NewCodecTransport wraps inner. preferred is CodecBatched (also when
// empty) or CodecLegacy; initiator is true on the side that speaks first.
func NewCodecTransport(inner Transport, preferred string, initiator bool) *CodecTransport {
	c := &CodecTransport{
		Transport:     inner,
		preferred:     preferred != CodecLegacy,
		initiator:     initiator,
		queue:         make(chan []byte, batchQueueDepth),
		lingerMs:      envInt("OPENFLUX_BATCH_LINGER_MS", defaultLingerMs),
		maxBatchBytes: min(envInt("OPENFLUX_BATCH_BYTES", defaultMaxBatchBytes), maxFrameBytes-65537),
		maxBatchCount: min(envInt("OPENFLUX_BATCH_COUNT", defaultMaxBatchCount), maxFrameRecords-1),
		stopCh:        make(chan struct{}),
	}
	c.batchNow.Store(c.preferred)
	utils.Debugf("[CODEC] created: preferred=%s initiator=%v (both framings accepted on receive)",
		codecName(c.preferred), initiator)
	return c
}

func codecName(batch bool) string {
	if batch {
		return CodecBatched
	}
	return CodecLegacy
}

// Current names the framing frames are sent in now.
func (c *CodecTransport) Current() string { return codecName(c.sendBatch()) }

func (c *CodecTransport) Start() error {
	c.lifecycle.Lock()
	defer c.lifecycle.Unlock()
	select {
	case <-c.stopCh:
		return fmt.Errorf("codec transport is stopped")
	default:
	}
	if c.running.Load() {
		return nil
	}
	if err := c.Transport.Start(); err != nil {
		return err
	}
	c.running.Store(true)
	go c.flushLoop()
	return nil
}

func (c *CodecTransport) Stop() error {
	c.lifecycle.Lock()
	defer c.lifecycle.Unlock()
	select {
	case <-c.stopCh:
		return nil
	default:
	}
	c.running.Store(false)
	c.stopOnce.Do(func() { close(c.stopCh) })
	return c.Transport.Stop()
}

// Send copies the packet and queues it. A full queue drops it (TCP
// retransmits, UDP loses the datagram).
func (c *CodecTransport) Send(data []byte) error {
	if !c.running.Load() {
		return fmt.Errorf("codec transport is not running")
	}
	if len(data) > 65535 {
		return fmt.Errorf("packet too large for a frame: %d bytes", len(data))
	}
	p := append([]byte(nil), data...)
	select {
	case c.queue <- p:
		return nil
	default:
		utils.Debugf("[CODEC] Send: QUEUE FULL, dropped %d bytes", len(p))
		return fmt.Errorf("codec queue full")
	}
}

func (c *CodecTransport) Receive(callback func([]byte)) {
	c.mu.Lock()
	c.userCb = callback
	c.mu.Unlock()
	c.Transport.Receive(c.receive)
}

func (c *CodecTransport) receive(data []byte) {
	if len(data) == 0 {
		return
	}
	var pkts [][]byte
	switch data[0] {
	case batchFormatVersion:
		p, err := decodeBatch(data)
		if err != nil {
			utils.Debugf("[CODEC] batch frame (%d bytes) undecodable: %v", len(data), err)
			return
		}
		pkts = p
		if !c.peerBatch.Swap(true) {
			utils.Debugf("[CODEC] peer sends batch-v2 frames: sending batched from now on")
		}
	case 0x00, CompressionMarker:
		if len(data) == 1 {
			// A lone 0x00 is a carrier keepalive (Volga sends one through
			// its relay), not a frame of the peer's codec.
			return
		}
		p, err := decompress(data)
		if err != nil {
			utils.Debugf("[CODEC] legacy frame (%d bytes) undecodable: %v", len(data), err)
			return
		}
		pkts = [][]byte{p}
		if !c.peerLegacy.Swap(true) && !c.peerBatch.Load() {
			utils.Debugf("[CODEC] peer sends legacy frames: sending legacy until it sends a batch frame")
		}
	default:
		n := c.unknown.Add(1)
		if utils.Throttled("codec.unknown", 30*time.Second) {
			utils.Infof("[CODEC] %d frame(s) from the peer in no classic framing (first bytes % x)%s",
				n, data[:min(len(data), 4)], layeringHint(data))
		}
		return
	}
	if !c.heard.Swap(true) {
		mode := c.Current()
		if c.initiator && mode != codecName(c.preferred) {
			utils.Infof("[CODEC] peer answered in %s framing (preferred was %s): staying on it", mode, codecName(c.preferred))
		} else {
			utils.Debugf("[CODEC] peer heard; sending %s", mode)
		}
	}
	c.mu.RLock()
	cb := c.userCb
	c.mu.RUnlock()
	if cb == nil {
		return
	}
	for _, p := range pkts {
		if len(p) > 0 {
			cb(p)
		}
	}
}

// layeringHint explains a frame that is neither framing.
func layeringHint(p []byte) string {
	if len(p) >= 3 && p[0] == encryptedMagic[0] && p[1] == encryptedMagic[1] && p[2] == encryptedMagic[2] {
		return ": an encrypted record outside any codec, i.e. the peer runs a Session (--negotiate / --transports / .conf / a Session profile)"
	}
	if p[0]>>4 == 4 {
		return ": a bare IPv4 packet (a peer without any codec)"
	}
	return ""
}

// sendBatch picks the framing for the next frame.
func (c *CodecTransport) sendBatch() bool {
	if c.peerBatch.Load() {
		return true
	}
	if c.peerLegacy.Load() {
		return false
	}
	if !c.initiator || c.heard.Load() {
		return c.batchNow.Load()
	}
	now := time.Now().UnixNano()
	first := c.firstSend.Load()
	if first == 0 {
		c.firstSend.CompareAndSwap(0, now)
		c.lastSwitch.Store(now)
		return c.batchNow.Load()
	}
	last := c.lastSwitch.Load()
	wait := codecFallbackEvery
	if last == first {
		wait = codecFirstFallback
	}
	if time.Duration(now-last) >= wait && c.lastSwitch.CompareAndSwap(last, now) {
		was := c.batchNow.Load()
		c.batchNow.Store(!was)
		if utils.Throttled("codec.fallback", 10*time.Second) {
			utils.Infof("[CODEC] no answer from the peer in %s framing; trying %s", codecName(was), codecName(!was))
		}
	}
	return c.batchNow.Load()
}

func (c *CodecTransport) flushLoop() {
	for c.running.Load() {
		var first []byte
		select {
		case <-c.stopCh:
			return
		case first = <-c.queue:
		}
		if !c.sendBatch() {
			c.write(compress(first))
			continue
		}
		batch := [][]byte{first}
		size := 2 + len(first)
	drain:
		for size < c.maxBatchBytes && len(batch) < c.maxBatchCount {
			select {
			case p := <-c.queue:
				batch = append(batch, p)
				size += 2 + len(p)
			default:
				break drain
			}
		}
		if c.lingerMs > 0 && size < c.maxBatchBytes && len(batch) < c.maxBatchCount {
			timer := time.NewTimer(time.Duration(c.lingerMs) * time.Millisecond)
		linger:
			for size < c.maxBatchBytes && len(batch) < c.maxBatchCount {
				select {
				case <-c.stopCh:
					timer.Stop()
					return
				case p := <-c.queue:
					batch = append(batch, p)
					size += 2 + len(p)
				case <-timer.C:
					break linger
				}
			}
			timer.Stop()
		}
		c.write(encodeBatch(batch))
	}
}

func (c *CodecTransport) write(frame []byte) {
	if err := c.Transport.Send(frame); err != nil {
		n := c.sendErrors.Add(1)
		utils.Debugf("[CODEC] send error (total=%d): %v", n, err)
	}
}

// SendErrors counts Transport.Send failures seen by the codec.
func (c *CodecTransport) SendErrors() uint64 { return c.sendErrors.Load() }
