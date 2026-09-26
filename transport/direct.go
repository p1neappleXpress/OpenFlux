package transport

import (
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"openflux/utils"
)

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
	}
}

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

	// Outbound queue. Direct writes into the socket are cheap, but we still
	// need a queue so a slow peer doesn't block the caller (e.g. gVisor).
	queue chan []byte

	// Exit-mode listener.
	listener net.Listener

	// Stats not tracked by BaseTransport directly.
	reconnects atomic.Uint64
	drops      atomic.Uint64

	// Counters for verbose logging.
	bytesIn   atomic.Uint64
	bytesOut  atomic.Uint64
	recordsIn atomic.Uint64
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
	}
	utils.Debugf("[DIRECT] created: isExit=%v listen=%q dial=%q handshakeTimeout=%v readTimeout=%v keepAlive=%v maxRecord=%d queueCap=%d",
		cfg.IsExit, cfg.ListenAddr, cfg.DialAddr, cfg.HandshakeTimeout, cfg.ReadTimeout,
		cfg.KeepAliveInterval, cfg.MaxRecordBytes, base.MaxQueueSize)
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
// A full queue drops the datagram; upper layers (TCP) retransmit, UDP loses.
func (t *DirectTransport) Send(data []byte) error {
	if !t.IsRunning() {
		utils.Debugf("[DIRECT] Send: not running, dropping %d bytes", len(data))
		return fmt.Errorf("direct: not running")
	}
	if len(data) == 0 || len(data) > t.config.MaxRecordBytes {
		utils.Debugf("[DIRECT] Send: bad size %d (max %d)", len(data), t.config.MaxRecordBytes)
		return fmt.Errorf("direct: record size %d outside 1..%d", len(data), t.config.MaxRecordBytes)
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	select {
	case t.queue <- cp:
		utils.Debugf("[DIRECT] Send: enqueued %d bytes (queue %d/%d)",
			len(cp), len(t.queue), cap(t.queue))
		if utils.IsVerbose() {
			utils.Debugf("[DIRECT] Send hexdump (%d bytes):\n%s", len(cp), hex.Dump(cp))
		}
		return nil
	default:
		t.drops.Add(1)
		utils.Debugf("[DIRECT] Send: QUEUE FULL, dropped %d bytes (drops=%d)",
			len(cp), t.drops.Load())
		return fmt.Errorf("direct: queue full")
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
		d := net.Dialer{Timeout: t.config.HandshakeTimeout}
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

	buf := make([]byte, t.config.MaxRecordBytes)
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
		utils.Debugf("[DIRECT] serveConn: waiting for 2-byte header")
		if _, err := io.ReadFull(conn, buf[:2]); err != nil {
			if err == io.EOF {
				utils.Debugf("[DIRECT] serveConn: peer closed (EOF) while reading header")
			} else {
				utils.Debugf("[DIRECT] serveConn: read header failed: %v", err)
			}
			return
		}
		n := int(buf[0])<<8 | int(buf[1])
		utils.Debugf("[DIRECT] serveConn: header says record length=%d", n)
		if n == 0 || n > t.config.MaxRecordBytes {
			utils.Debugf("[DIRECT] serveConn: INVALID record length %d (max %d), closing",
				n, t.config.MaxRecordBytes)
			return
		}
		utils.Debugf("[DIRECT] serveConn: reading body (%d bytes)", n)
		if _, err := io.ReadFull(conn, buf[:n]); err != nil {
			utils.Debugf("[DIRECT] serveConn: read body failed after %d/%d bytes: %v",
				len(buf[:n]), n, err)
			return
		}
		utils.Debugf("[DIRECT] serveConn: received record #%d size=%d",
			t.recordsIn.Load()+1, n)
		if utils.IsVerbose() {
			utils.Debugf("[DIRECT] serveConn: record hexdump:\n%s", hex.Dump(buf[:n]))
		}

		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		t.RecordReceive(n)
		t.bytesIn.Add(uint64(n))
		t.recordsIn.Add(1)
		t.CallReceive(pkt)
	}
}

// ---- writer ----

func (t *DirectTransport) writerLoop() {
	utils.Debugf("[DIRECT] writerLoop: started")
	var pending []byte
	for {
		if pending == nil {
			select {
			case <-t.done:
				utils.Debugf("[DIRECT] writerLoop: done signal, exiting")
				return
			case pkt := <-t.queue:
				pending = pkt
				utils.Debugf("[DIRECT] writerLoop: dequeued %d bytes (queue %d/%d)",
					len(pkt), len(t.queue), cap(t.queue))
			}
		}

		t.mu.RLock()
		conn := t.conn
		t.mu.RUnlock()
		if conn == nil {
			utils.Debugf("[DIRECT] writerLoop: no active conn, holding %d bytes; retrying in 20ms",
				len(pending))
			select {
			case <-t.done:
				utils.Debugf("[DIRECT] writerLoop: done during wait, exiting")
				return
			case <-time.After(20 * time.Millisecond):
			}
			continue
		}

		if len(pending) > t.config.MaxRecordBytes {
			utils.Debugf("[DIRECT] writerLoop: dropping oversized record %d (max %d)",
				len(pending), t.config.MaxRecordBytes)
			pending = nil
			continue
		}
		hdr := [2]byte{byte(len(pending) >> 8), byte(len(pending))}
		utils.Debugf("[DIRECT] writerLoop: writing record #%d size=%d (hdr=%02x%02x)",
			t.recordsOut.Load()+1, len(pending), hdr[0], hdr[1])
		if utils.IsVerbose() {
			utils.Debugf("[DIRECT] writerLoop: record hexdump:\n%s", hex.Dump(pending))
		}

		t.writeMu.Lock()
		_, err := conn.Write(hdr[:])
		if err == nil {
			_, err = conn.Write(pending)
		}
		t.writeMu.Unlock()

		if err != nil {
			utils.Debugf("[DIRECT] writerLoop: WRITE FAILED for %s: %v (holding record, retry in 20ms)",
				connDesc(conn), err)
			select {
			case <-t.done:
				return
			case <-time.After(20 * time.Millisecond):
			}
			continue // keep pending; reconnect will bring up a new conn
		}
		utils.Debugf("[DIRECT] writerLoop: wrote record size=%d OK", len(pending))
		t.RecordSend(len(pending))
		t.bytesOut.Add(uint64(len(pending)))
		t.recordsOut.Add(1)
		pending = nil
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
