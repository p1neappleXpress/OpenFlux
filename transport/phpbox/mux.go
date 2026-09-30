package phpbox

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/p1neappleXpress/OpenFlux/network"
	"github.com/p1neappleXpress/OpenFlux/utils"
)

// Carrier is a bidirectional byte-stream link the mux rides on. Its shape is
// a subset of transport.Transport (Start/Stop/Send/Receive), so the real
// cupsonline (or any OpenFlux) transport is a Carrier as-is, and so is the
// direct-HTTP two-channel link (see client.go) or a test pipe. Send/Receive
// carry arbitrary bytes; the mux frames its own messages, so a carrier need
// not preserve message boundaries.
type Carrier interface {
	Start() error
	Stop() error
	Send([]byte) error
	Receive(func([]byte))
}

// ErrSessionClosed is returned by Dial after the mux has been closed or its
// carrier has ended.
var ErrSessionClosed = errors.New("phpbox: session closed")

// Mux multiplexes TCP streams to arbitrary destinations over one Carrier,
// speaking the OPEN/DATA/CLOSE frame protocol to a phpbox exit. Dial returns
// a net.Conn per logical stream. This is the client role; the exit is
// deploy/phpbox (PHP over cups) or a Go stream exit for interop tests.
type Mux struct {
	carrier Carrier

	mu      sync.Mutex
	streams map[uint32]*conn
	nextID  uint32
	dead    bool
	closed  chan struct{}

	rxMu  sync.Mutex
	rxBuf []byte

	// onClosed, if set, is called once when the carrier ends on its own.
	onClosed func()

	// Flow control (see FrameAck). Windows bound how much DATA is in flight, per stream and in
	// all, so a saturated carrier cannot bury small frames (a TLS handshake, a CLOSE) behind
	// megabytes of bulk: the carrier's own queue (a document server's, a socket buffer) is what
	// grows otherwise, and every other stream waits behind it.
	fcMu         sync.Mutex
	streamWindow uint32            // bytes one stream may have unacknowledged; 0 = no flow control
	totalWindow  uint32            // the same over all streams (a stream may always have one frame out)
	totalUp      int64             // unacknowledged bytes over all streams we send on
	fcEvt        chan struct{}     // closed and replaced whenever an ack or a close may unblock a writer
	ackQ         map[uint32]uint32 // stream -> consumed total, waiting to be sent
	ackSig       chan struct{}
}

// Default windows: about a second of a 300-500 KB/s carrier, in flight at most.
const (
	defaultStreamWindow = 256 << 10
	defaultTotalWindow  = 512 << 10
	ackEvery            = 32 << 10 // a stream acks after this much was consumed (or when its buffer runs dry)
	maxFrame            = 65536    // fewer, larger carrier messages: the carriers charge per message, not per byte
	ackStallAfter       = 10 * time.Second
)

// SetFlowControl sets the per-stream and total windows in bytes; 0 turns flow
// control off (streams then behave as before acks existed). Call before Dial.
func (m *Mux) SetFlowControl(stream, total int) {
	m.fcMu.Lock()
	m.streamWindow, m.totalWindow = uint32(stream), uint32(total)
	m.fcMu.Unlock()
}

// NewMux wires the mux to carrier's receive callback. Call Start to bring the
// carrier up.
func NewMux(carrier Carrier) *Mux {
	m := &Mux{
		carrier: carrier,
		streams: map[uint32]*conn{},
		nextID:  1,
		closed:  make(chan struct{}),

		streamWindow: defaultStreamWindow,
		totalWindow:  defaultTotalWindow,
		fcEvt:        make(chan struct{}),
		ackQ:         map[uint32]uint32{},
		ackSig:       make(chan struct{}, 1),
	}
	carrier.Receive(m.onBytes)
	go m.ackLoop()
	return m
}

// Start brings the carrier up.
func (m *Mux) Start() error { return m.carrier.Start() }

// Dial opens a stream to host:port through the exit.
func (m *Mux) Dial(ctx context.Context, host string, port int) (net.Conn, error) {
	m.mu.Lock()
	if m.dead {
		m.mu.Unlock()
		return nil, ErrSessionClosed
	}
	id := m.nextID
	m.nextID++
	st := newConn(m, id)
	m.streams[id] = st
	m.mu.Unlock()

	utils.Debugf("[STREAM] stream %d: dialing %s:%d", id, host, port)
	open := net.JoinHostPort(host, strconv.Itoa(port))
	m.fcMu.Lock()
	wantFC := m.streamWindow > 0
	m.fcMu.Unlock()
	if wantFC {
		open += "\x00fc" // we understand acks; an exit that does answers OPEN_OK "fc"
	}
	if err := m.sendFrame(Frame{Type: FrameOpen, StreamID: id, Payload: []byte(open)}); err != nil {
		m.dropStream(id)
		return nil, fmt.Errorf("phpbox: open %s:%d: %w", host, port, err)
	}

	select {
	case res := <-st.openRes:
		if res != "" {
			utils.Debugf("[STREAM] stream %d: open %s:%d refused: %s", id, host, port, res)
			m.dropStream(id)
			return nil, fmt.Errorf("phpbox: open %s:%d: %s", host, port, res)
		}
		utils.Debugf("[STREAM] stream %d: open %s:%d", id, host, port)
		return st, nil
	case <-ctx.Done():
		utils.Debugf("[STREAM] stream %d: dial %s:%d abandoned: %v", id, host, port, ctx.Err())
		m.dropStream(id)
		return nil, ctx.Err()
	case <-m.closed:
		return nil, ErrSessionClosed
	}
}

// Close ends the mux and its carrier and shuts down every open stream.
func (m *Mux) Close() error {
	m.mu.Lock()
	if m.dead {
		m.mu.Unlock()
		return nil
	}
	m.dead = true
	streams := m.streams
	m.streams = map[uint32]*conn{}
	close(m.closed)
	m.mu.Unlock()

	err := m.carrier.Stop()
	for _, st := range streams {
		st.shutdown()
	}
	return err
}

// sendPatience is how long a frame waits for a busy or reconnecting carrier
// before the send is given up on. A frame dropped silently would corrupt the
// stream (a lost byte inside a TLS record breaks the handshake), so the
// caller is told instead.
const sendPatience = 15 * time.Second

// sendFrame hands f to the carrier, waiting (with backoff) while the carrier
// pushes back - its write queue full, or the link reconnecting - instead of
// dropping the frame. It returns an error only when the mux is closed or the
// carrier stayed unusable for sendPatience.
func (m *Mux) sendFrame(f Frame) error {
	b := Encode(nil, f)
	deadline := time.Now().Add(sendPatience)
	wait := time.Millisecond
	var lastLog time.Time
	for {
		select {
		case <-m.closed:
			return ErrSessionClosed
		default:
		}
		err := m.carrier.Send(b)
		if err == nil {
			logFrame(network.DirOutbound, f)
			return nil
		}
		if time.Now().After(deadline) {
			utils.Infof("[STREAM] stream %d: carrier unusable for %v (%v); dropping the %s frame", f.StreamID, sendPatience, err, frameName(f.Type))
			return err
		}
		if time.Since(lastLog) > 2*time.Second { // one line per stall, not per retry
			utils.Debugf("[STREAM] stream %d: carrier busy (%v); holding the %s frame", f.StreamID, err, frameName(f.Type))
			lastLog = time.Now()
		}
		time.Sleep(wait)
		if wait < 50*time.Millisecond {
			wait *= 2
		}
	}
}

// onBytes receives arbitrary byte chunks from the carrier, reassembles frames
// across chunk boundaries, and dispatches them.
func (m *Mux) onBytes(b []byte) {
	m.rxMu.Lock()
	m.rxBuf = append(m.rxBuf, b...)
	var frames []Frame
	for {
		f, rest, ok := TakeFrame(m.rxBuf)
		if !ok {
			break
		}
		m.rxBuf = rest
		frames = append(frames, f)
	}
	m.rxMu.Unlock()
	for _, f := range frames {
		m.dispatch(f)
	}
}

func (m *Mux) dispatch(f Frame) {
	logFrame(network.DirInbound, f)
	m.mu.Lock()
	st := m.streams[f.StreamID]
	m.mu.Unlock()
	if st == nil {
		utils.Debugf("[STREAM] stream %d: %s for a stream that is gone (dropped)", f.StreamID, frameName(f.Type))
		return
	}
	switch f.Type {
	case FrameOpenOK:
		if bytes.Contains(f.Payload, []byte("fc")) {
			st.fc.Store(true) // the exit honours acks on this stream: we must too, both ways
		}
		st.signalOpen("")
	case FrameAck:
		if len(f.Payload) == 4 {
			m.onAck(st, binary.BigEndian.Uint32(f.Payload))
		}
	case FrameOpenErr:
		st.signalOpen(string(f.Payload))
	case FrameData:
		st.deliver(f.Payload)
	case FrameClose:
		st.remoteClosed()
	}
}

func (m *Mux) dropStream(id uint32) {
	m.mu.Lock()
	st := m.streams[id]
	delete(m.streams, id)
	m.mu.Unlock()
	if st != nil {
		m.fcForget(st)
		st.shutdown()
	}
}

// markClosed is called when the carrier ends on its own (not via Close): mark
// the mux dead and shut down every open stream so callers unblock.
func (m *Mux) markClosed() {
	m.mu.Lock()
	if m.dead {
		m.mu.Unlock()
		return
	}
	m.dead = true
	streams := m.streams
	m.streams = map[uint32]*conn{}
	close(m.closed)
	m.mu.Unlock()

	if m.onClosed != nil {
		m.onClosed()
	}
	for _, st := range streams {
		st.shutdown()
	}
}

// conn is one logical stream; it satisfies net.Conn.
type conn struct {
	m       *Mux
	id      uint32
	openRes chan string

	rbuf     rbuffer
	closed   atomic.Bool
	openOnce sync.Once

	// Flow control: fc once the exit agreed; the counters are guarded by m.fcMu and are running
	// totals (uint32, wrapping), so a lost ack is made good by the next one.
	fc        atomic.Bool
	sent      uint32 // DATA bytes we wrote on this stream
	acked     uint32 // how many of those the exit says it consumed
	consumed  uint32 // DATA bytes the app read from this stream
	ackedSent uint32 // the value of consumed we last told the exit
}

func newConn(m *Mux, id uint32) *conn {
	st := &conn{m: m, id: id, openRes: make(chan string, 1)}
	st.rbuf.cond = sync.NewCond(&st.rbuf.mu)
	st.rbuf.onRead = st.noteConsumed
	return st
}

func (s *conn) signalOpen(res string) { s.openOnce.Do(func() { s.openRes <- res }) }
func (s *conn) deliver(p []byte)      { s.rbuf.write(p) }
func (s *conn) remoteClosed() {
	utils.Debugf("[STREAM] stream %d: closed by the exit", s.id)
	s.rbuf.close()
}

// shutdown ends the stream locally without sending CLOSE (the whole mux died).
func (s *conn) shutdown() {
	s.closed.Store(true)
	s.rbuf.close()
	s.signalOpen("session closed")
	s.m.fcMu.Lock()
	s.m.fcWake()
	s.m.fcMu.Unlock()
}

func (s *conn) Read(p []byte) (int, error) { return s.rbuf.read(p) }

func (s *conn) Write(p []byte) (int, error) {
	if s.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	for off := 0; off < len(p); {
		end := off + maxFrame
		if end > len(p) {
			end = len(p)
		}
		if err := s.waitWindow(uint32(end - off)); err != nil {
			return off, err
		}
		if err := s.m.sendFrame(Frame{Type: FrameData, StreamID: s.id, Payload: append([]byte(nil), p[off:end]...)}); err != nil {
			return off, err
		}
		s.m.fcSent(s, uint32(end-off))
		off = end
	}
	return len(p), nil
}

// waitWindow blocks while the stream (or all streams together) already has a
// window of unacknowledged data out. A stream with nothing in flight always
// may send one frame, so progress is guaranteed; and if no ack arrives for
// ackStallAfter the acks are assumed lost and the stream carries on.
func (s *conn) waitWindow(n uint32) error {
	if !s.fc.Load() {
		return nil
	}
	m := s.m
	var stalled time.Time
	for {
		m.fcMu.Lock()
		inflight := s.sent - s.acked
		ok := inflight == 0 || (inflight+n <= m.streamWindow && m.totalUp+int64(n) <= int64(m.totalWindow))
		if ok {
			m.fcMu.Unlock()
			return nil
		}
		if stalled.IsZero() {
			stalled = time.Now()
		} else if time.Since(stalled) > ackStallAfter {
			utils.Infof("[STREAM] stream %d: no ack for %v with %d bytes in flight; assuming they were lost", s.id, ackStallAfter, inflight)
			m.totalUp -= int64(s.sent - s.acked)
			s.acked = s.sent
			m.fcMu.Unlock()
			return nil
		}
		evt := m.fcEvt
		m.fcMu.Unlock()
		select {
		case <-evt:
		case <-time.After(time.Second):
		case <-m.closed:
			return ErrSessionClosed
		}
		if s.closed.Load() {
			return io.ErrClosedPipe
		}
	}
}

// fcSent records DATA we just sent.
func (m *Mux) fcSent(s *conn, n uint32) {
	if !s.fc.Load() {
		return
	}
	m.fcMu.Lock()
	s.sent += n
	m.totalUp += int64(n)
	m.fcMu.Unlock()
}

// onAck applies the exit's running total of consumed bytes for a stream.
func (m *Mux) onAck(s *conn, v uint32) {
	m.fcMu.Lock()
	delta := v - s.acked
	if delta != 0 && delta <= s.sent-s.acked { // never beyond what was sent (a stale ack)
		s.acked = v
		m.totalUp -= int64(delta)
		m.fcWake()
	}
	m.fcMu.Unlock()
}

// fcForget drops a finished stream's unacknowledged bytes from the total.
func (m *Mux) fcForget(s *conn) {
	m.fcMu.Lock()
	m.totalUp -= int64(s.sent - s.acked)
	s.acked = s.sent
	m.fcWake()
	m.fcMu.Unlock()
}

// fcWake releases every writer waiting on a window. Caller holds fcMu.
func (m *Mux) fcWake() {
	close(m.fcEvt)
	m.fcEvt = make(chan struct{})
}

// noteConsumed is called after the app read n bytes from the stream; empty says
// its buffer ran dry. It queues an ack when enough was consumed, or when the
// buffer is empty (so the exit keeps the pipe full instead of waiting).
func (s *conn) noteConsumed(n int, empty bool) {
	if !s.fc.Load() || n <= 0 {
		return
	}
	m := s.m
	m.fcMu.Lock()
	s.consumed += uint32(n)
	due := s.consumed - s.ackedSent
	if due >= ackEvery || (empty && due > 0) {
		s.ackedSent = s.consumed
		m.ackQ[s.id] = s.consumed
		select {
		case m.ackSig <- struct{}{}:
		default:
		}
	}
	m.fcMu.Unlock()
}

// ackLoop sends the queued acks off the readers' path (a busy carrier must not
// block an app's Read) and coalesces: only the latest total per stream is sent.
func (m *Mux) ackLoop() {
	for {
		select {
		case <-m.ackSig:
		case <-m.closed:
			return
		}
		m.fcMu.Lock()
		q := m.ackQ
		m.ackQ = map[uint32]uint32{}
		m.fcMu.Unlock()
		for id, v := range q {
			var p [4]byte
			binary.BigEndian.PutUint32(p[:], v)
			if err := m.sendFrame(Frame{Type: FrameAck, StreamID: id, Payload: p[:]}); err != nil {
				if errors.Is(err, ErrSessionClosed) {
					return
				}
			}
		}
	}
}

func (s *conn) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	utils.Debugf("[STREAM] stream %d: closed by the app", s.id)
	_ = s.m.sendFrame(Frame{Type: FrameClose, StreamID: s.id})
	s.m.dropStream(s.id)
	s.rbuf.close()
	return nil
}

type phpboxAddr struct{ s string }

func (a phpboxAddr) Network() string { return "phpbox" }
func (a phpboxAddr) String() string  { return a.s }

func (s *conn) LocalAddr() net.Addr                { return phpboxAddr{"phpbox-client"} }
func (s *conn) RemoteAddr() net.Addr               { return phpboxAddr{"phpbox-exit"} }
func (s *conn) SetDeadline(t time.Time) error      { return nil }
func (s *conn) SetReadDeadline(t time.Time) error  { return nil }
func (s *conn) SetWriteDeadline(t time.Time) error { return nil }

// rbuffer is a blocking byte buffer fed by deliver and drained by Read.
type rbuffer struct {
	mu   sync.Mutex
	cond *sync.Cond
	buf  []byte
	eof  bool

	// onRead, if set, is told how much a Read took and whether the buffer is now empty.
	onRead func(n int, empty bool)
}

func (r *rbuffer) write(p []byte) {
	r.mu.Lock()
	if !r.eof {
		r.buf = append(r.buf, p...)
		r.cond.Signal()
	}
	r.mu.Unlock()
}

func (r *rbuffer) close() {
	r.mu.Lock()
	r.eof = true
	r.cond.Broadcast()
	r.mu.Unlock()
}

func (r *rbuffer) read(p []byte) (int, error) {
	r.mu.Lock()
	for len(r.buf) == 0 {
		if r.eof {
			r.mu.Unlock()
			return 0, io.EOF
		}
		r.cond.Wait()
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	empty := len(r.buf) == 0
	r.mu.Unlock()
	if r.onRead != nil {
		r.onRead(n, empty)
	}
	return n, nil
}
