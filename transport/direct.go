package transport

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/p1neappleXpress/OpenFlux/netbind"
	"github.com/p1neappleXpress/OpenFlux/utils"
)

// ErrRecordTooLarge is what a carrier answers for a record it can never carry (as
// opposed to one it cannot carry now): the batcher drops such a frame instead of
// retrying it for ever.
var ErrRecordTooLarge = errors.New("record too large for the carrier")

// DirectConfig configures the DirectTransport.
//
// DirectTransport is a plain TCP byte-stream carrier between a client and an
// exit node. It carries *already framed* tunnel packets (the same wire that
// BatchedTransport / NegotiatedTransport produce), so every packet on the wire
// is exactly one length-prefixed record:
//
//	[2-byte big-endian length][payload]
//
// It is intended to be wrapped by EncryptedTransport (AES-256-GCM) on both
// sides. main.go MUST refuse to start --transport=direct without a key.
type DirectConfig struct {
	// ListenAddr is the local address the exit node binds to.
	// Empty or ":0" means "any free port on all interfaces".
	// Always set to 0.0.0.0:PORT when running as an exit node reachable from
	// the internet, so the socket is not loopback-only.
	ListenAddr string

	// DialAddr is the remote address the client dials.
	// "host:port". Ignored when ListenAddr is set (server mode).
	DialAddr string

	// IsExit chooses listen-vs-dial. Exactly one of the two must be true on
	// each peer.
	IsExit bool

	// HandshakeTimeout bounds the initial TCP connect / accept.
	HandshakeTimeout time.Duration

	// ReadTimeout bounds a single ReadMessage on the wire. A zero value
	// disables the deadline and relies on TCP keepalives.
	ReadTimeout time.Duration

	// KeepAliveInterval controls the TCP keepalive probe interval on the
	// underlying net.Conn. Zero disables keepalive.
	KeepAliveInterval time.Duration

	// ReconnectMin / ReconnectMax / ReconnectMultiplier control the client
	// reconnect backoff. The exit node does not reconnect; it accepts.
	ReconnectMinDelay   time.Duration
	ReconnectMaxDelay   time.Duration
	ReconnectMultiplier float64

	// MaxRecordBytes caps a single framed record (2-byte length prefix means
	// 65535 is the hard ceiling).
	MaxRecordBytes int

	// Unpreferred turns off the Session's preference for this carrier (see
	// PreferredCarrier): its profile priority then decides, as for any other.
	Unpreferred bool

	// QueueBytes bounds the framed bytes waiting for the socket (0: only the
	// record count of the queue bounds it). Whatever sits in this queue is
	// ahead of every later packet, so a deep queue is added latency: an
	// aborted download leaves its buffered tail in front of the next page.
	QueueBytes int
}

// DefaultDirectConfig returns conservative defaults matching the rest of the
// project's style.
func DefaultDirectConfig() DirectConfig {
	return DirectConfig{
		HandshakeTimeout:    15 * time.Second,
		ReadTimeout:         0, // rely on TCP keepalive
		KeepAliveInterval:   30 * time.Second,
		ReconnectMinDelay:   200 * time.Millisecond,
		ReconnectMaxDelay:   15 * time.Second,
		ReconnectMultiplier: 1.4,
		MaxRecordBytes:      65535,
		QueueBytes:          envInt("OPENFLUX_DIRECT_QUEUE_KB", directQueueBytes>>10) << 10,
	}
}

const (
	// A burst of queued records goes out in one writev; the bounds keep one
	// write from holding the carrier for long.
	directCoalesceRecords = 32
	directCoalesceBytes   = 256 << 10
	// The receive side reads the socket through a buffer this big: one
	// system call serves many records.
	directReadBuffer = 128 << 10
	// The outbound byte budget. The socket has its own send buffer; this
	// only has to cover scheduling jitter between the packet source and the
	// writer, and the less of it there is the less stale data an aborted
	// transfer leaves in front of fresh packets.
	directQueueBytes = 512 << 10
	// How long Send waits for budget before it drops the record.
	directSendWait = 50 * time.Millisecond

	// directNotSentLowatDefault bounds the unsent data the kernel queues for the
	// carrier socket (Linux, TCP_NOTSENT_LOWAT).
	directNotSentLowatDefault = 256 << 10
)

// DirectTransport is a minimal TCP carrier for already-framed tunnel records.
//
// Client mode dials DialAddr and reconnects on drop.
// Exit mode listens on ListenAddr and accepts a single active peer at a time.
//
// The transport does not encrypt, compress, or batch. Wrap it with
// EncryptedTransport (and optionally NegotiatedTransport) in main.go.
type DirectTransport struct {
	*BaseTransport

	config DirectConfig

	// Active connection. Swap under mu.
	mu   sync.RWMutex
	conn net.Conn

	// Accept loop / dial loop lifecycle.
	done chan struct{}
	once sync.Once

	// Send serialization: one writer at a time.
	writeMu sync.Mutex

	// Outbound queue of framed records ([2-byte length][payload]). A slow
	// peer must not block the caller (e.g. gVisor) for long.
	queue       chan []byte
	queuedBytes atomic.Int64
	space       chan struct{} // the writer signals freed budget

	// Exit-mode listener.
	listener net.Listener

	// Stats not tracked by BaseTransport directly.
	reconnects atomic.Uint64
	drops      atomic.Uint64

	// Counters for verbose logging.
	bytesIn    atomic.Uint64
	bytesOut   atomic.Uint64
	recordsIn  atomic.Uint64
	recordsOut atomic.Uint64
}

// NewDirectTransport builds a DirectTransport from a base config and a
// transport.DirectConfig.
//
// The caller is expected to have already chosen IsExit and DialAddr/ListenAddr.
func NewDirectTransport(base TransportConfig, cfg DirectConfig) *DirectTransport {
	t := &DirectTransport{
		BaseTransport: NewBaseTransport(base),
		config:        cfg,
		done:          make(chan struct{}),
		queue:         make(chan []byte, base.MaxQueueSize),
		space:         make(chan struct{}, 1),
	}
	utils.Debugf("[DIRECT] created: isExit=%v listen=%q dial=%q handshakeTimeout=%v readTimeout=%v keepAlive=%v maxRecord=%d queueCap=%d queueBytes=%d",
		cfg.IsExit, cfg.ListenAddr, cfg.DialAddr, cfg.HandshakeTimeout, cfg.ReadTimeout,
		cfg.KeepAliveInterval, cfg.MaxRecordBytes, base.MaxQueueSize, cfg.QueueBytes)
	return t
}

// Start launches the accept (exit) or dial (client) loop.
func (t *DirectTransport) Start() error {
	utils.Debugf("[DIRECT] Start() called: isExit=%v", t.config.IsExit)
	if err := t.BaseTransport.Start(); err != nil {
		utils.Debugf("[DIRECT] Start: BaseTransport.Start failed: %v", err)
		return err
	}

	if t.config.IsExit {
		addr := t.config.ListenAddr
		if addr == "" {
			addr = "0.0.0.0:0"
		}
		utils.Debugf("[DIRECT] exit: binding listener on %s", addr)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			utils.Debugf("[DIRECT] exit: listen %s failed: %v", addr, err)
			return fmt.Errorf("direct: listen %s: %w", addr, err)
		}
		t.listener = ln
		utils.Debugf("[DIRECT] exit listening on %s (waiting for a client)", ln.Addr().String())
		go t.acceptLoop()
	} else {
		if t.config.DialAddr == "" {
			utils.Debugf("[DIRECT] client: DialAddr is empty, refusing to start")
			return fmt.Errorf("direct: DialAddr is empty")
		}
		utils.Debugf("[DIRECT] client: starting dial loop to %s", t.config.DialAddr)
		go t.dialLoop()
	}

	go t.writerLoop()
	utils.Debugf("[DIRECT] Start: loops launched")
	return nil
}

// Stop closes the listener, the active connection and the outbound queue.
func (t *DirectTransport) Stop() error {
	utils.Debugf("[DIRECT] Stop() called")
	t.once.Do(func() {
		close(t.done)
	})
	t.mu.Lock()
	if t.listener != nil {
		utils.Debugf("[DIRECT] Stop: closing listener %s", t.listener.Addr().String())
		_ = t.listener.Close()
		t.listener = nil
	}
	if t.conn != nil {
		utils.Debugf("[DIRECT] Stop: closing active conn %s", connDesc(t.conn))
		_ = t.conn.Close()
		t.conn = nil
	}
	t.mu.Unlock()
	t.SetConnected(false)
	return t.BaseTransport.Stop()
}

// Send enqueues an already-framed record for delivery. The payload MUST be a
// complete wire frame as expected by the peer (e.g. the output of
// encodeBatch / encodeBatchV3 / NegotiatedTransport's envelope).
//
// The record is copied once, together with its 2-byte length prefix, so the
// writer puts it on the socket with no further copying. When the outbound
// queue is full Send waits a short while for the socket to drain (so the
// caller feels backpressure instead of losing the record) and only then
// drops it; upper layers (TCP) retransmit, UDP loses.
func (t *DirectTransport) Send(data []byte) error {
	if !t.IsRunning() {
		if utils.DebugEnabled() {
			utils.Debugf("[DIRECT] Send: not running, dropping %d bytes", len(data))
		}
		return fmt.Errorf("direct: not running")
	}
	if len(data) == 0 || len(data) > t.config.MaxRecordBytes {
		if utils.DebugEnabled() {
			utils.Debugf("[DIRECT] Send: bad size %d (max %d)", len(data), t.config.MaxRecordBytes)
		}
		return fmt.Errorf("direct: record size %d outside 1..%d: %w", len(data), t.config.MaxRecordBytes, ErrRecordTooLarge)
	}
	rec := make([]byte, 2+len(data))
	rec[0], rec[1] = byte(len(data)>>8), byte(len(data))
	copy(rec[2:], data)
	return t.enqueue(rec, len(data))
}

// enqueue claims budget for a framed record and queues it, dropping it when
// the queue stays full. n is the payload size.
func (t *DirectTransport) enqueue(rec []byte, n int) error {
	if !t.reserve(len(rec)) {
		t.drops.Add(1)
		if utils.DebugEnabled() {
			utils.Debugf("[DIRECT] Send: QUEUE FULL (bytes), dropped %d bytes (drops=%d)", n, t.drops.Load())
		}
		return fmt.Errorf("direct: queue full")
	}
	select {
	case t.queue <- rec:
		if utils.IsVerbose() {
			utils.Debugf("[DIRECT] Send hexdump (%d bytes):\n%s", n, hex.Dump(rec[2:]))
		}
		return nil
	default:
		t.queuedBytes.Add(-int64(len(rec)))
		t.drops.Add(1)
		if utils.DebugEnabled() {
			utils.Debugf("[DIRECT] Send: QUEUE FULL, dropped %d bytes (drops=%d)", n, t.drops.Load())
		}
		return fmt.Errorf("direct: queue full")
	}
}

// SendWithHeadroom is Send for a record that already has RecordHeadroom spare
// bytes in front (HeadroomSender): the length prefix is written into them and
// the buffer is queued as it is, with no copy.
func (t *DirectTransport) SendWithHeadroom(buf []byte) error {
	n := len(buf) - RecordHeadroom
	if !t.IsRunning() {
		return fmt.Errorf("direct: not running")
	}
	if n <= 0 || n > t.config.MaxRecordBytes {
		return fmt.Errorf("direct: record size %d outside 1..%d: %w", n, t.config.MaxRecordBytes, ErrRecordTooLarge)
	}
	buf[0], buf[1] = byte(n>>8), byte(n)
	return t.enqueue(buf, n)
}

// reserve claims size bytes of the outbound byte budget, waiting up to
// directSendWait for the writer to free some.
func (t *DirectTransport) reserve(size int) bool {
	limit := int64(t.config.QueueBytes)
	if limit <= 0 {
		t.queuedBytes.Add(int64(size))
		return true
	}
	var deadline time.Time
	for {
		cur := t.queuedBytes.Load()
		if cur == 0 || cur+int64(size) <= limit {
			if t.queuedBytes.CompareAndSwap(cur, cur+int64(size)) {
				return true
			}
			continue
		}
		if deadline.IsZero() {
			deadline = time.Now().Add(directSendWait)
		}
		left := time.Until(deadline)
		if left <= 0 {
			return false
		}
		select {
		case <-t.space:
		case <-t.done:
			return false
		case <-time.After(min(left, 5*time.Millisecond)):
		}
	}
}

// Receive installs the user callback. DirectTransport forwards every framed
// record it reads from the wire.
func (t *DirectTransport) Receive(cb func([]byte)) {
	utils.Debugf("[DIRECT] Receive: callback installed")
	t.BaseTransport.Receive(cb)
}

// IsConnected reports whether a live TCP connection exists.
func (t *DirectTransport) IsConnected() bool {
	return t.BaseTransport.IsConnected()
}

// Stats returns counters plus reconnect/drop counts specific to DirectTransport.
func (t *DirectTransport) Stats() TransportStats {
	s := t.BaseTransport.Stats()
	s.Reconnects = t.reconnects.Load()
	s.Connected = t.IsConnected()
	return s
}

// Drops returns the number of Send calls dropped because the queue was full.
func (t *DirectTransport) Drops() uint64 { return t.drops.Load() }

// Preferred implements PreferredCarrier. OPENFLUX_DIRECT_FIRST=0 turns the
// preference off for every direct carrier.
func (t *DirectTransport) Preferred() bool {
	return !t.config.Unpreferred && os.Getenv("OPENFLUX_DIRECT_FIRST") != "0"
}

// CheapBytes marks a plain TCP connection: its bytes cost less than the CPU
// that would shrink them (see CheapBytesCarrier).
func (t *DirectTransport) CheapBytes() bool { return true }

// directCarrierMTU is the tunnel MTU a direct carrier offers: a TCP stream moves
// a 16 KB packet as it moves a 1.5 KB one, and every layer above works per
// packet. 16000 plus the envelope, batch and AES overhead stays under the
// 16 KiB that every message-oriented carrier is safe with, should a later
// one join. OPENFLUX_DIRECT_MTU=1500 restores the old MTU.
var directCarrierMTU = func() int {
	if n, err := strconv.Atoi(os.Getenv("OPENFLUX_DIRECT_MTU")); err == nil && n >= 1280 && n <= 32000 {
		return n
	}
	return 16000
}()

// CarrierMTU implements CarrierMTUer.
func (t *DirectTransport) CarrierMTU() int { return directCarrierMTU }

// ---- exit mode ----

func (t *DirectTransport) acceptLoop() {
	utils.Debugf("[DIRECT] acceptLoop: started on %s", t.listener.Addr().String())
	for {
		select {
		case <-t.done:
			utils.Debugf("[DIRECT] acceptLoop: done signal, exiting")
			return
		default:
		}

		utils.Debugf("[DIRECT] acceptLoop: blocking on Accept()")
		conn, err := t.listener.Accept()
		if err != nil {
			select {
			case <-t.done:
				utils.Debugf("[DIRECT] acceptLoop: Accept failed after close: %v", err)
				return
			default:
			}
			utils.Debugf("[DIRECT] acceptLoop: Accept error: %v", err)
			continue
		}
		utils.Debugf("[DIRECT] acceptLoop: ACCEPTED %s", connDesc(conn))
		t.serveConn(conn)
		utils.Debugf("[DIRECT] acceptLoop: serveConn returned, looping back")
	}
}

// ---- client mode ----

func (t *DirectTransport) dialLoop() {
	delay := t.config.ReconnectMinDelay
	if delay <= 0 {
		delay = 200 * time.Millisecond
	}
	utils.Debugf("[DIRECT] dialLoop: started, initial delay=%v", delay)
	attempt := 0
	for {
		select {
		case <-t.done:
			utils.Debugf("[DIRECT] dialLoop: done signal, exiting")
			return
		default:
		}

		attempt++
		utils.Debugf("[DIRECT] dialLoop: attempt #%d dialing %s (timeout=%v)",
			attempt, t.config.DialAddr, t.config.HandshakeTimeout)
		d := netbind.DialerFor(t.GetConfig().Network, t.config.HandshakeTimeout)
		start := time.Now()
		conn, err := d.Dial("tcp", t.config.DialAddr)
		elapsed := time.Since(start)
		if err != nil {
			utils.Debugf("[DIRECT] dialLoop: DIAL FAILED after %v: %v", elapsed, err)
		} else {
			utils.Debugf("[DIRECT] dialLoop: CONNECTED to %s in %v (local=%s remote=%s)",
				t.config.DialAddr, elapsed, connDesc(conn),
				conn.RemoteAddr().String())
			t.serveConn(conn)
			t.reconnects.Add(1)
			utils.Debugf("[DIRECT] dialLoop: serveConn returned (reconnects=%d)", t.reconnects.Load())
			delay = t.config.ReconnectMinDelay
			if delay <= 0 {
				delay = 200 * time.Millisecond
			}
		}

		utils.Debugf("[DIRECT] dialLoop: waiting %v before next attempt", delay)
		select {
		case <-t.done:
			utils.Debugf("[DIRECT] dialLoop: done signal during wait, exiting")
			return
		case <-time.After(delay):
		}
		delay = time.Duration(float64(delay) * t.config.ReconnectMultiplier)
		if delay > t.config.ReconnectMaxDelay {
			delay = t.config.ReconnectMaxDelay
		}
	}
}

// ---- shared conn lifecycle ----

func (t *DirectTransport) serveConn(conn net.Conn) {
	utils.Debugf("[DIRECT] serveConn: begin %s", connDesc(conn))
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
		if t.config.KeepAliveInterval > 0 {
			_ = tc.SetKeepAlive(true)
			_ = tc.SetKeepAlivePeriod(t.config.KeepAliveInterval)
			utils.Debugf("[DIRECT] serveConn: TCP no-delay + keepalive=%v set", t.config.KeepAliveInterval)
		}
		setNotSentLowat(conn, directNotSentLowat)
	}

	t.mu.Lock()
	t.conn = conn
	t.mu.Unlock()
	t.SetConnected(true)
	utils.Debugf("[DIRECT] serveConn: connected=%v", t.IsConnected())

	defer func() {
		utils.Debugf("[DIRECT] serveConn: tearing down %s", connDesc(conn))
		t.mu.Lock()
		if t.conn == conn {
			t.conn = nil
		}
		t.mu.Unlock()
		t.SetConnected(false)
		_ = conn.Close()
	}()

	// A buffered reader turns two reads per record (length, body) into one
	// read per many records: at the packet rates of a busy tunnel that is
	// most of the receive side's system calls.
	br := bufio.NewReaderSize(conn, directReadBuffer)
	var hdr [2]byte
	for {
		select {
		case <-t.done:
			utils.Debugf("[DIRECT] serveConn: done signal, exiting read loop")
			return
		default:
		}

		if t.config.ReadTimeout > 0 {
			_ = conn.SetReadDeadline(time.Now().Add(t.config.ReadTimeout))
		}
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			if err == io.EOF {
				utils.Debugf("[DIRECT] serveConn: peer closed (EOF) while reading header")
			} else {
				utils.Debugf("[DIRECT] serveConn: read header failed: %v", err)
			}
			return
		}
		n := int(hdr[0])<<8 | int(hdr[1])
		if n == 0 || n > t.config.MaxRecordBytes {
			utils.Debugf("[DIRECT] serveConn: INVALID record length %d (max %d), closing",
				n, t.config.MaxRecordBytes)
			return
		}
		// The record is read straight into the buffer that is handed upwards.
		pkt := make([]byte, n)
		if _, err := io.ReadFull(br, pkt); err != nil {
			utils.Debugf("[DIRECT] serveConn: read body failed (want %d bytes): %v", n, err)
			return
		}
		if utils.IsVerbose() {
			utils.Debugf("[DIRECT] serveConn: record hexdump:\n%s", hex.Dump(pkt))
		}
		t.RecordReceive(n)
		t.bytesIn.Add(uint64(n))
		t.recordsIn.Add(1)
		t.CallReceive(pkt)
	}
}

// ---- writer ----

func (t *DirectTransport) writerLoop() {
	utils.Debugf("[DIRECT] writerLoop: started")
	var batch [][]byte // framed records ([len][payload]) not yet fully written
	var out net.Buffers
	for {
		if len(batch) == 0 {
			select {
			case <-t.done:
				utils.Debugf("[DIRECT] writerLoop: done signal, exiting")
				return
			case rec := <-t.queue:
				batch = append(batch, rec)
			}
			size := len(batch[0])
		drain:
			for len(batch) < directCoalesceRecords && size < directCoalesceBytes {
				select {
				case rec := <-t.queue:
					batch = append(batch, rec)
					size += len(rec)
				default:
					break drain
				}
			}
		}

		t.mu.RLock()
		conn := t.conn
		t.mu.RUnlock()
		if conn == nil {
			utils.Debugf("[DIRECT] writerLoop: no active conn, holding %d records; retrying in 20ms", len(batch))
			select {
			case <-t.done:
				utils.Debugf("[DIRECT] writerLoop: done during wait, exiting")
				return
			case <-time.After(20 * time.Millisecond):
			}
			continue
		}

		out = append(out[:0], batch...) // WriteTo consumes its slice
		t.writeMu.Lock()
		_, err := out.WriteTo(conn)
		t.writeMu.Unlock()

		if err != nil {
			// A write may have gone out in part: this socket can no longer be framed. Close it so
			// the batch is retried only on a new connection.
			_ = conn.Close()
			utils.Debugf("[DIRECT] writerLoop: WRITE FAILED for %s: %v (holding %d records, retry in 20ms)",
				connDesc(conn), err, len(batch))
			select {
			case <-t.done:
				return
			case <-time.After(20 * time.Millisecond):
			}
			continue // keep the batch; a reconnect brings a new conn
		}
		var freed int64
		for _, rec := range batch {
			n := len(rec) - 2
			t.RecordSend(n)
			t.bytesOut.Add(uint64(n))
			t.recordsOut.Add(1)
			freed += int64(len(rec))
			putBuf(rec) // written: the carrier owned it from SendWithHeadroom
		}
		t.queuedBytes.Add(-freed)
		select {
		case t.space <- struct{}{}:
		default:
		}
		for i := range batch {
			batch[i] = nil
		}
		batch = batch[:0]
	}
}

// connDesc returns a short descriptor for a net.Conn for logging.
func connDesc(c net.Conn) string {
	if c == nil {
		return "<nil>"
	}
	local := "?"
	remote := "?"
	if a := c.LocalAddr(); a != nil {
		local = a.String()
	}
	if a := c.RemoteAddr(); a != nil {
		remote = a.String()
	}
	return fmt.Sprintf("%s->%s", local, remote)
}

// directNotSentLowat is the unsent-data bound of the carrier socket, in bytes
// (OPENFLUX_DIRECT_LOWAT_KB; 0 leaves the kernel's autotuning alone).
var directNotSentLowat = func() int {
	if v := os.Getenv("OPENFLUX_DIRECT_LOWAT_KB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n << 10
		}
	}
	return directNotSentLowatDefault
}()
