package phpbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Client dials TCP streams to arbitrary destinations through a phpbox exit.
// One Client owns one session (one down-poll); Dial returns a net.Conn per
// logical stream. When the exit caps the down-poll the session ends: open
// streams see io.EOF and further Dials fail with ErrSessionClosed until the
// caller makes a new Client (epoch rotation is left to the caller in v0).
type Client struct {
	endpoint string // e.g. https://host/phpbox.php
	token    string
	session  string
	http     *http.Client

	ctx    context.Context
	cancel context.CancelFunc
	upCh   chan Frame
	closed chan struct{}
	stopUp chan struct{}

	mu      sync.Mutex
	streams map[uint32]*conn
	nextID  uint32
	dead    bool
}

// ErrSessionClosed is returned by Dial after the down-poll has ended.
var ErrSessionClosed = errors.New("phpbox: session closed")

// NewClient starts a session against endpoint (the URL of phpbox.php). The
// down-poll runs until Close, the exit caps it, or it errors.
func NewClient(endpoint, token, session string) *Client {
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{
		endpoint: endpoint,
		token:    token,
		session:  session,
		http:     &http.Client{Timeout: 0}, // the down-poll is long-lived
		ctx:      ctx,
		cancel:   cancel,
		upCh:     make(chan Frame, 256),
		closed:   make(chan struct{}),
		stopUp:   make(chan struct{}),
		streams:  map[uint32]*conn{},
		nextID:   1,
	}
	go c.downLoop()
	go c.upLoop()
	return c
}

func (c *Client) url(role string) string {
	v := url.Values{"k": {c.token}, "s": {c.session}, "r": {role}}
	return c.endpoint + "?" + v.Encode()
}

// Dial opens a stream to host:port through the exit.
func (c *Client) Dial(ctx context.Context, host string, port int) (net.Conn, error) {
	c.mu.Lock()
	if c.dead {
		c.mu.Unlock()
		return nil, ErrSessionClosed
	}
	id := c.nextID
	c.nextID++
	st := newConn(c, id)
	c.streams[id] = st
	c.mu.Unlock()

	c.send(Frame{Type: FrameOpen, StreamID: id, Payload: []byte(net.JoinHostPort(host, strconv.Itoa(port)))})

	select {
	case res := <-st.openRes:
		if res != "" {
			c.dropStream(id)
			return nil, fmt.Errorf("phpbox: open %s:%d: %s", host, port, res)
		}
		return st, nil
	case <-ctx.Done():
		c.dropStream(id)
		return nil, ctx.Err()
	case <-c.closed:
		return nil, ErrSessionClosed
	}
}

// Close ends the session and all its streams.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.dead {
		c.mu.Unlock()
		return nil
	}
	c.dead = true
	streams := c.streams
	c.streams = map[uint32]*conn{}
	c.mu.Unlock()

	c.cancel()
	close(c.closed)
	close(c.stopUp)
	for _, st := range streams {
		st.shutdown()
	}
	return nil
}

func (c *Client) send(f Frame) {
	select {
	case c.upCh <- f:
	case <-c.closed:
	}
}

func (c *Client) dropStream(id uint32) {
	c.mu.Lock()
	st := c.streams[id]
	delete(c.streams, id)
	c.mu.Unlock()
	if st != nil {
		st.shutdown()
	}
}

// upLoop batches queued frames and POSTs them to the exit's up route. The
// exit buffers request bodies, so each POST is short and self-contained.
func (c *Client) upLoop() {
	t := time.NewTicker(20 * time.Millisecond)
	defer t.Stop()
	var buf []byte
	flush := func() {
		if len(buf) == 0 {
			return
		}
		req, err := http.NewRequestWithContext(c.ctx, http.MethodPost, c.url("up"), bytes.NewReader(buf))
		if err == nil {
			req.Header.Set("Content-Type", "application/octet-stream")
			if resp, err := c.http.Do(req); err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}
		buf = buf[:0]
	}
	for {
		select {
		case f := <-c.upCh:
			buf = Encode(buf, f)
			for drained := false; !drained; {
				select {
				case f2 := <-c.upCh:
					buf = Encode(buf, f2)
				default:
					drained = true
				}
			}
		case <-t.C:
			flush()
		case <-c.stopUp:
			flush()
			return
		}
	}
}

// downLoop holds the streaming GET and dispatches inbound frames until the
// exit closes it (the ~140s cap) or an error. Then the session is marked
// dead and every open stream is shut down.
func (c *Client) downLoop() {
	defer c.markDead()
	req, err := http.NewRequestWithContext(c.ctx, http.MethodGet, c.url("down"), nil)
	if err != nil {
		return
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	var buf []byte
	tmp := make([]byte, 32768)
	for {
		n, rerr := resp.Body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			for {
				f, rest, ok := TakeFrame(buf)
				if !ok {
					break
				}
				buf = rest
				c.dispatch(f)
			}
		}
		if rerr != nil {
			return
		}
	}
}

func (c *Client) dispatch(f Frame) {
	c.mu.Lock()
	st := c.streams[f.StreamID]
	c.mu.Unlock()
	if st == nil {
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

func (c *Client) markDead() {
	c.mu.Lock()
	if c.dead {
		c.mu.Unlock()
		return
	}
	c.dead = true
	streams := c.streams
	c.streams = map[uint32]*conn{}
	c.mu.Unlock()

	c.cancel()

	select {
	case <-c.closed:
	default:
		close(c.closed)
	}
	select {
	case <-c.stopUp:
	default:
		close(c.stopUp)
	}
	for _, st := range streams {
		st.shutdown()
	}
}

// conn is one logical stream; it satisfies net.Conn.
type conn struct {
	c       *Client
	id      uint32
	openRes chan string

	rbuf     bytes.Buffer
	rmu      sync.Mutex
	rcond    *sync.Cond
	eof      bool
	closed   atomic.Bool
	openOnce sync.Once
}

func newConn(c *Client, id uint32) *conn {
	st := &conn{c: c, id: id, openRes: make(chan string, 1)}
	st.rcond = sync.NewCond(&st.rmu)
	return st
}

func (s *conn) signalOpen(res string) {
	s.openOnce.Do(func() { s.openRes <- res })
}

func (s *conn) deliver(p []byte) {
	s.rmu.Lock()
	if !s.eof {
		s.rbuf.Write(p)
		s.rcond.Signal()
	}
	s.rmu.Unlock()
}

func (s *conn) remoteClosed() {
	s.rmu.Lock()
	s.eof = true
	s.rcond.Broadcast()
	s.rmu.Unlock()
}

// shutdown marks the stream ended locally without sending a CLOSE (used when
// the whole session dies).
func (s *conn) shutdown() {
	s.closed.Store(true)
	s.remoteClosed()
	s.signalOpen("session closed")
}

func (s *conn) Read(p []byte) (int, error) {
	s.rmu.Lock()
	defer s.rmu.Unlock()
	for s.rbuf.Len() == 0 {
		if s.eof {
			return 0, io.EOF
		}
		s.rcond.Wait()
	}
	return s.rbuf.Read(p)
}

func (s *conn) Write(p []byte) (int, error) {
	if s.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	// Chunk so a single frame stays a reasonable size on the wire.
	const max = 32768
	for off := 0; off < len(p); off += max {
		end := off + max
		if end > len(p) {
			end = len(p)
		}
		s.c.send(Frame{Type: FrameData, StreamID: s.id, Payload: append([]byte(nil), p[off:end]...)})
	}
	return len(p), nil
}

func (s *conn) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	s.c.send(Frame{Type: FrameClose, StreamID: s.id})
	s.c.dropStream(s.id)
	s.remoteClosed()
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
