package phpbox

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// sharedQueueLink models a document server: ONE FIFO for both directions, a
// fixed bandwidth, and no limit on how much may wait in it (bufferbloat). Small
// frames queue behind whatever bulk came before them.
type sharedQueueLink struct {
	bps    float64
	mu     sync.Mutex
	cond   *sync.Cond
	q      []linkItem
	closed bool

	toClient func([]byte)
	toExit   func([]byte)
}

type linkItem struct {
	toExit bool
	data   []byte
}

func newSharedQueueLink(bps float64) *sharedQueueLink {
	l := &sharedQueueLink{bps: bps}
	l.cond = sync.NewCond(&l.mu)
	go l.run()
	return l
}

func (l *sharedQueueLink) put(toExit bool, b []byte) {
	l.mu.Lock()
	l.q = append(l.q, linkItem{toExit, append([]byte(nil), b...)})
	l.cond.Signal()
	l.mu.Unlock()
}

func (l *sharedQueueLink) run() {
	for {
		l.mu.Lock()
		for len(l.q) == 0 && !l.closed {
			l.cond.Wait()
		}
		if l.closed {
			l.mu.Unlock()
			return
		}
		it := l.q[0]
		l.q = l.q[1:]
		l.mu.Unlock()
		time.Sleep(time.Duration(float64(len(it.data)) / l.bps * float64(time.Second)))
		if it.toExit {
			l.toExit(it.data)
		} else {
			l.toClient(it.data)
		}
	}
}

func (l *sharedQueueLink) close() { l.mu.Lock(); l.closed = true; l.cond.Broadcast(); l.mu.Unlock() }

// clientSide is the mux's Carrier on the link.
type clientSide struct {
	l    *sharedQueueLink
	recv func([]byte)
}

func (c *clientSide) Receive(cb func([]byte)) { c.recv = cb }
func (c *clientSide) Start() error            { c.l.toClient = func(b []byte) { c.recv(b) }; return nil }
func (c *clientSide) Stop() error             { return nil }
func (c *clientSide) Send(b []byte) error     { c.l.put(true, b); return nil }

// fcExit is a Go exit that honours flow control the way deploy/phpbox does:
// it reads a destination only while the stream (and all streams) have less
// than a window unacknowledged, and acks the bytes it wrote to destinations.
type fcExit struct {
	l      *sharedQueueLink
	window uint32
	total  uint32
	fcOn   bool

	mu     sync.Mutex
	cond   *sync.Cond
	rx     []byte
	st     map[uint32]*fcStream
	inAll  int64
	nextFn func(Frame)
}

type fcStream struct {
	c          net.Conn
	sent       uint32
	acked      uint32
	fc         bool
	wrote      uint32
	ackedSent  uint32
	writeQueue chan []byte
}

func newFCExit(l *sharedQueueLink, fcOn bool) *fcExit {
	e := &fcExit{l: l, window: defaultStreamWindow, total: defaultTotalWindow, fcOn: fcOn, st: map[uint32]*fcStream{}}
	e.cond = sync.NewCond(&e.mu)
	l.toExit = e.feed
	return e
}

func (e *fcExit) emit(f Frame) { e.l.put(false, Encode(nil, f)) }

func (e *fcExit) feed(b []byte) {
	e.mu.Lock()
	e.rx = append(e.rx, b...)
	var fs []Frame
	for {
		f, rest, ok := TakeFrame(e.rx)
		if !ok {
			break
		}
		e.rx = rest
		fs = append(fs, f)
	}
	e.mu.Unlock()
	for _, f := range fs {
		e.apply(f)
	}
}

func (e *fcExit) apply(f Frame) {
	switch f.Type {
	case FrameOpen:
		parts := strings.SplitN(string(f.Payload), "\x00", 2)
		go func() {
			c, err := net.DialTimeout("tcp", parts[0], 3*time.Second)
			if err != nil {
				e.emit(Frame{Type: FrameOpenErr, StreamID: f.StreamID, Payload: []byte(err.Error())})
				return
			}
			s := &fcStream{c: c, fc: e.fcOn && len(parts) == 2 && strings.Contains(parts[1], "fc"), writeQueue: make(chan []byte, 1024)}
			e.mu.Lock()
			e.st[f.StreamID] = s
			e.mu.Unlock()
			ok := Frame{Type: FrameOpenOK, StreamID: f.StreamID}
			if s.fc {
				ok.Payload = []byte("fc")
			}
			e.emit(ok)
			go e.reader(f.StreamID, s)
			go e.writer(f.StreamID, s)
		}()
	case FrameData:
		e.mu.Lock()
		s := e.st[f.StreamID]
		e.mu.Unlock()
		if s != nil {
			s.writeQueue <- f.Payload
		}
	case FrameAck:
		e.mu.Lock()
		if s := e.st[f.StreamID]; s != nil && len(f.Payload) == 4 {
			v := binary.BigEndian.Uint32(f.Payload)
			if d := v - s.acked; d != 0 && d <= s.sent-s.acked {
				s.acked = v
				e.inAll -= int64(d)
				e.cond.Broadcast()
			}
		}
		e.mu.Unlock()
	case FrameClose:
		e.mu.Lock()
		s := e.st[f.StreamID]
		delete(e.st, f.StreamID)
		e.mu.Unlock()
		if s != nil {
			s.c.Close()
			close(s.writeQueue)
		}
	}
}

func (e *fcExit) writer(id uint32, s *fcStream) {
	for p := range s.writeQueue {
		s.c.Write(p)
		if !s.fc {
			continue
		}
		e.mu.Lock()
		s.wrote += uint32(len(p))
		due := s.wrote - s.ackedSent
		idle := len(s.writeQueue) == 0
		if due >= ackEvery || (idle && due > 0) {
			s.ackedSent = s.wrote
			v := s.wrote
			e.mu.Unlock()
			var b [4]byte
			binary.BigEndian.PutUint32(b[:], v)
			e.emit(Frame{Type: FrameAck, StreamID: id, Payload: b[:]})
			continue
		}
		e.mu.Unlock()
	}
}

func (e *fcExit) reader(id uint32, s *fcStream) {
	buf := make([]byte, maxFrame)
	for {
		if s.fc {
			e.mu.Lock()
			for (s.sent-s.acked >= e.window || e.inAll >= int64(e.total)) && s.sent != s.acked {
				e.cond.Wait()
			}
			e.mu.Unlock()
		}
		n, err := s.c.Read(buf)
		if n > 0 {
			e.emit(Frame{Type: FrameData, StreamID: id, Payload: append([]byte(nil), buf[:n]...)})
			if s.fc {
				e.mu.Lock()
				s.sent += uint32(n)
				e.inAll += int64(n)
				e.mu.Unlock()
			}
		}
		if err != nil {
			e.emit(Frame{Type: FrameClose, StreamID: id})
			return
		}
	}
}

// bulkServer streams n bytes to every connection as fast as it is read, and echoes small
// requests on another listener.
func bulkServer(t *testing.T, n int) net.Listener {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				chunk := make([]byte, 32768)
				for sent := 0; sent < n; sent += len(chunk) {
					if _, err := c.Write(chunk); err != nil {
						return
					}
				}
			}()
		}
	}()
	return ln
}

func fcEchoServer(t *testing.T) net.Listener {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	return ln
}

func portOf(ln net.Listener) (string, int) {
	h, p, _ := net.SplitHostPort(ln.Addr().String())
	n := 0
	for _, ch := range p {
		n = n*10 + int(ch-'0')
	}
	return h, n
}

// smallExchanges measures how long a handshake-sized round trip (open, send a
// little, get it back) takes while bulk downloads run, in milliseconds.
func smallExchanges(t *testing.T, fcOn bool) (median time.Duration, bulkDone bool) {
	link := newSharedQueueLink(3 << 20) // 3 MB/s shared by both directions
	defer link.close()
	cs := &clientSide{l: link}
	m := NewMux(cs)
	if !fcOn {
		m.SetFlowControl(0, 0)
	}
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	newFCExit(link, fcOn)

	bulk := bulkServer(t, 1<<30) // effectively endless: it is stopped when the mux closes
	defer bulk.Close()
	echo := fcEchoServer(t)
	defer echo.Close()
	bh, bp := portOf(bulk)
	eh, ep := portOf(echo)

	var done atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			c, err := m.Dial(ctx, bh, bp)
			if err != nil {
				return
			}
			defer c.Close()
			n, _ := io.Copy(io.Discard, c)
			if n > 0 {
				done.Add(1)
			}
		}()
	}
	time.Sleep(1500 * time.Millisecond) // let the bulk fill the pipe

	var lat []time.Duration
	const give = 6 * time.Second
	for i := 0; i < 3; i++ {
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), give)
		ok := false
		c, err := m.Dial(ctx, eh, ep)
		if err == nil {
			c.Write([]byte("hello-handshake-sized"))
			buf := make([]byte, 32)
			got := make(chan struct{})
			go func() { c.Read(buf); close(got) }()
			select {
			case <-got:
				ok = true
			case <-ctx.Done():
			}
			c.Close()
		}
		cancel()
		if ok {
			lat = append(lat, time.Since(start))
		} else {
			lat = append(lat, give) // it never got through: as bad as the wait we allowed
		}
		time.Sleep(200 * time.Millisecond)
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	m.Close()
	wg.Wait()
	return lat[len(lat)/2], done.Load() > 0
}

// The reason streams starve when a link is saturated: without windows, bulk
// fills the carrier's queue and every small frame waits behind it. With them,
// the queue is bounded by the windows and a handshake stays quick.
func TestFlowControlKeepsHandshakesQuickUnderBulk(t *testing.T) {
	if testing.Short() {
		t.Skip("saturates a simulated link for several seconds")
	}
	withFC, _ := smallExchanges(t, true)
	without, _ := smallExchanges(t, false)
	t.Logf("median small round trip under 4 bulk downloads: with flow control %v, without %v", withFC, without)
	if withFC > 1500*time.Millisecond {
		t.Errorf("with flow control a small round trip took %v, want under 1.5s", withFC)
	}
	if without < 2*withFC {
		t.Errorf("without flow control %v was not clearly worse than with %v: the test does not show the problem it guards against", without, withFC)
	}
}

// An exit from before flow control never sends "fc"; the client must not wait
// for acks that never come.
func TestNoFlowControlAgainstAnOldExit(t *testing.T) {
	pc := newPipeCarrier() // goStreamExit: ignores the suffix, answers OPEN_OK with no caps
	m := NewMux(pc)
	_ = m.Start()
	defer m.Close()
	echo := fcEchoServer(t)
	defer echo.Close()
	h, p := portOf(echo)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := m.Dial(ctx, h, p)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	payload := strings.Repeat("x", 3*defaultStreamWindow) // far more than one window, no acks will ever arrive
	done := make(chan error, 1)
	go func() { _, err := c.Write([]byte(payload)); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Write blocked waiting for acks from an exit that never sends them")
	}
}
