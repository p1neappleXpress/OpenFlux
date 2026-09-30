package phpbox

import (
	"context"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// pipeCarrier links a Mux to an in-process Go stream exit over a byte pipe,
// so the mux is tested over a generic Carrier (like cupsonline would be),
// not only over HTTP.
type pipeCarrier struct {
	recv func([]byte)
	exit *goStreamExit
	stop chan struct{}
	once sync.Once
}

func newPipeCarrier() *pipeCarrier {
	return &pipeCarrier{exit: newGoStreamExit(), stop: make(chan struct{})}
}

func (p *pipeCarrier) Receive(cb func([]byte)) { p.recv = cb }
func (p *pipeCarrier) Start() error {
	p.exit.toClient = func(b []byte) {
		if p.recv != nil {
			p.recv(b)
		}
	}
	return nil
}
func (p *pipeCarrier) Stop() error { p.once.Do(func() { close(p.stop) }); return nil }
func (p *pipeCarrier) Send(b []byte) error {
	select {
	case <-p.stop:
	default:
		p.exit.feed(b)
	}
	return nil
}

// goStreamExit is a Go port of the phpbox exit demux for tests: reassemble
// frames, dial dsts, pump bytes back as frames.
type goStreamExit struct {
	mu       sync.Mutex
	rx       []byte
	conns    map[uint32]net.Conn
	toClient func([]byte)
}

func newGoStreamExit() *goStreamExit { return &goStreamExit{conns: map[uint32]net.Conn{}} }

func (e *goStreamExit) emit(f Frame) {
	if e.toClient != nil {
		e.toClient(Encode(nil, f))
	}
}

func (e *goStreamExit) feed(b []byte) {
	e.mu.Lock()
	e.rx = append(e.rx, b...)
	var frames []Frame
	for {
		f, rest, ok := TakeFrame(e.rx)
		if !ok {
			break
		}
		e.rx = rest
		frames = append(frames, f)
	}
	e.mu.Unlock()
	for _, f := range frames {
		e.apply(f)
	}
}

func (e *goStreamExit) apply(f Frame) {
	switch f.Type {
	case FrameOpen:
		// An exit from before flow control: it does not know the "\x00fc" suffix a client appends to
		// OPEN, so it must tolerate it the way phpbox.php does, and it answers OPEN_OK with no caps.
		c, err := net.DialTimeout("tcp", strings.SplitN(string(f.Payload), "\x00", 2)[0], 3*time.Second)
		if err != nil {
			e.emit(Frame{Type: FrameOpenErr, StreamID: f.StreamID, Payload: []byte(err.Error())})
			return
		}
		e.mu.Lock()
		e.conns[f.StreamID] = c
		e.mu.Unlock()
		e.emit(Frame{Type: FrameOpenOK, StreamID: f.StreamID})
		go e.reader(f.StreamID, c)
	case FrameData:
		e.mu.Lock()
		c := e.conns[f.StreamID]
		e.mu.Unlock()
		if c != nil {
			c.Write(f.Payload)
		}
	case FrameClose:
		e.mu.Lock()
		c := e.conns[f.StreamID]
		delete(e.conns, f.StreamID)
		e.mu.Unlock()
		if c != nil {
			c.Close()
		}
	}
}

func (e *goStreamExit) reader(id uint32, c net.Conn) {
	b := make([]byte, 32768)
	for {
		n, err := c.Read(b)
		if n > 0 {
			e.emit(Frame{Type: FrameData, StreamID: id, Payload: append([]byte(nil), b[:n]...)})
		}
		if err != nil {
			e.emit(Frame{Type: FrameClose, StreamID: id})
			return
		}
	}
}

func muxDial(t *testing.T, m *Mux, addr string) net.Conn {
	t.Helper()
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := m.Dial(ctx, host, port)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	return c
}

func TestMuxOverPipeEcho(t *testing.T) {
	echo := echoServer(t)
	defer echo.Close()

	m := NewMux(newPipeCarrier())
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	conn := muxDial(t, m, echo.Addr().String())
	msg := "carrier-agnostic mux round-trip"
	if _, err := conn.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != msg {
		t.Fatalf("echo: got %q want %q", got, msg)
	}
}

func TestMuxOverPipeOpenError(t *testing.T) {
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := closed.Addr().String()
	closed.Close()

	m := NewMux(newPipeCarrier())
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := m.Dial(ctx, host, port); err == nil {
		t.Fatal("dial to a closed port should fail")
	} else if !strings.Contains(err.Error(), "open") {
		t.Fatalf("unexpected error: %v", err)
	}
}
