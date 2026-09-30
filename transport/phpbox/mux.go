package phpbox

import (
	"context"
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
}

// NewMux wires the mux to carrier's receive callback. Call Start to bring the
// carrier up.
func NewMux(carrier Carrier) *Mux {
	m := &Mux{
		carrier: carrier,
		streams: map[uint32]*conn{},
		nextID:  1,
		closed:  make(chan struct{}),
	}
	carrier.Receive(m.onBytes)
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
	if err := m.sendFrame(Frame{Type: FrameOpen, StreamID: id, Payload: []byte(net.JoinHostPort(host, strconv.Itoa(port)))}); err != nil {
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
		st.signalOpen("")
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
}

func newConn(m *Mux, id uint32) *conn {
	st := &conn{m: m, id: id, openRes: make(chan string, 1)}
	st.rbuf.cond = sync.NewCond(&st.rbuf.mu)
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
}

func (s *conn) Read(p []byte) (int, error) { return s.rbuf.read(p) }

func (s *conn) Write(p []byte) (int, error) {
	if s.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	const max = 65536 // fewer, larger carrier messages: the carriers charge per message, not per byte
	for off := 0; off < len(p); off += max {
		end := off + max
		if end > len(p) {
			end = len(p)
		}
		if err := s.m.sendFrame(Frame{Type: FrameData, StreamID: s.id, Payload: append([]byte(nil), p[off:end]...)}); err != nil {
			return off, err
		}
	}
	return len(p), nil
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
	defer r.mu.Unlock()
	for len(r.buf) == 0 {
		if r.eof {
			return 0, io.EOF
		}
		r.cond.Wait()
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}
