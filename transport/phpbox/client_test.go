package phpbox

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeExit is an in-process stand-in for deploy/phpbox/phpbox.php: the same
// two-channel mux (down streams frames, up appends them) so the client
// protocol is exercised in CI without PHP. It dials real (loopback) dsts.
type fakeExit struct {
	mu   sync.Mutex
	sess map[string]*fakeSession
}

type fakeSession struct {
	mu     sync.Mutex
	upBuf  []byte
	downCh chan Frame
	conns  map[uint32]net.Conn
	once   sync.Once
}

func newFakeExit() *fakeExit { return &fakeExit{sess: map[string]*fakeSession{}} }

func (e *fakeExit) session(id string) *fakeSession {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.sess[id]
	if s == nil {
		s = &fakeSession{downCh: make(chan Frame, 256), conns: map[uint32]net.Conn{}}
		e.sess[id] = s
	}
	return s
}

func (e *fakeExit) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s := e.session(q.Get("s"))
	switch q.Get("r") {
	case "up":
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.upBuf = append(s.upBuf, body...)
		s.mu.Unlock()
		w.Write([]byte("ok"))
	case "down":
		s.pump(w, r)
	default:
		http.Error(w, "bad role", 400)
	}
}

func (s *fakeSession) pump(w http.ResponseWriter, r *http.Request) {
	fl, _ := w.(http.Flusher)
	go s.ingest(r.Context())
	deadline := time.After(10 * time.Second)
	for {
		select {
		case f := <-s.downCh:
			w.Write(Encode(nil, f))
			if fl != nil {
				fl.Flush()
			}
		case <-r.Context().Done():
			return
		case <-deadline:
			return
		}
	}
}

// ingest drains the upstream bus and applies OPEN/DATA/CLOSE, mirroring the
// PHP pump loop.
func (s *fakeSession) ingest(ctx context.Context) {
	s.once.Do(func() {
		t := time.NewTicker(5 * time.Millisecond)
		defer t.Stop()
		var buf []byte
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			s.mu.Lock()
			if len(s.upBuf) > 0 {
				buf = append(buf, s.upBuf...)
				s.upBuf = s.upBuf[:0]
			}
			s.mu.Unlock()
			for {
				f, rest, ok := TakeFrame(buf)
				if !ok {
					break
				}
				buf = rest
				s.apply(f)
			}
		}
	})
}

func (s *fakeSession) apply(f Frame) {
	switch f.Type {
	case FrameOpen:
		host, port, err := net.SplitHostPort(strings.SplitN(string(f.Payload), "\x00", 2)[0])
		_ = host
		_ = port
		if err != nil {
			s.downCh <- Frame{Type: FrameOpenErr, StreamID: f.StreamID, Payload: []byte("bad addr")}
			return
		}
		c, err := net.DialTimeout("tcp", strings.SplitN(string(f.Payload), "\x00", 2)[0], 3*time.Second)
		if err != nil {
			s.downCh <- Frame{Type: FrameOpenErr, StreamID: f.StreamID, Payload: []byte(err.Error())}
			return
		}
		s.mu.Lock()
		s.conns[f.StreamID] = c
		s.mu.Unlock()
		s.downCh <- Frame{Type: FrameOpenOK, StreamID: f.StreamID}
		go s.reader(f.StreamID, c)
	case FrameData:
		s.mu.Lock()
		c := s.conns[f.StreamID]
		s.mu.Unlock()
		if c != nil {
			c.Write(f.Payload)
		}
	case FrameClose:
		s.mu.Lock()
		c := s.conns[f.StreamID]
		delete(s.conns, f.StreamID)
		s.mu.Unlock()
		if c != nil {
			c.Close()
		}
	}
}

func (s *fakeSession) reader(id uint32, c net.Conn) {
	b := make([]byte, 32768)
	for {
		n, err := c.Read(b)
		if n > 0 {
			s.downCh <- Frame{Type: FrameData, StreamID: id, Payload: append([]byte(nil), b[:n]...)}
		}
		if err != nil {
			s.downCh <- Frame{Type: FrameClose, StreamID: id}
			return
		}
	}
}

// echoServer accepts TCP and echoes bytes back.
func echoServer(t *testing.T) net.Listener {
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
			go func(c net.Conn) { io.Copy(c, c); c.Close() }(c)
		}
	}()
	return ln
}

func dialPort(t *testing.T, c *Client, addr string) net.Conn {
	host, portStr, _ := net.SplitHostPort(addr)
	port := 0
	for _, ch := range portStr {
		port = port*10 + int(ch-'0')
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := c.Dial(ctx, host, port)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	return conn
}

func TestPhpboxEchoThroughMux(t *testing.T) {
	echo := echoServer(t)
	defer echo.Close()
	srv := httptest.NewServer(newFakeExit())
	defer srv.Close()

	c := NewClient(srv.URL, "tok", "sess1")
	defer c.Close()

	conn := dialPort(t, c, echo.Addr().String())
	msg := "hello phpbox through the mux"
	if _, err := conn.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != msg {
		t.Fatalf("echo: got %q want %q", got, msg)
	}
}

func TestPhpboxMultiplexesStreams(t *testing.T) {
	echo := echoServer(t)
	defer echo.Close()
	srv := httptest.NewServer(newFakeExit())
	defer srv.Close()

	c := NewClient(srv.URL, "tok", "sess2")
	defer c.Close()

	a := dialPort(t, c, echo.Addr().String())
	b := dialPort(t, c, echo.Addr().String())

	for _, tc := range []struct {
		conn net.Conn
		msg  string
	}{{a, "stream-A-payload"}, {b, "stream-B-different"}} {
		if _, err := tc.conn.Write([]byte(tc.msg)); err != nil {
			t.Fatal(err)
		}
		got := make([]byte, len(tc.msg))
		if _, err := io.ReadFull(tc.conn, got); err != nil {
			t.Fatal(err)
		}
		if string(got) != tc.msg {
			t.Fatalf("mux: got %q want %q", got, tc.msg)
		}
	}
}

func TestPhpboxOpenErrorSurfaces(t *testing.T) {
	// A port nobody listens on: the exit reports OPEN_ERR, Dial fails.
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := closed.Addr().String()
	closed.Close()

	srv := httptest.NewServer(newFakeExit())
	defer srv.Close()
	c := NewClient(srv.URL, "tok", "sess3")
	defer c.Close()

	host, portStr, _ := net.SplitHostPort(addr)
	port := 0
	for _, ch := range portStr {
		port = port*10 + int(ch-'0')
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.Dial(ctx, host, port); err == nil {
		t.Fatal("dial to a closed port should fail")
	} else if !strings.Contains(err.Error(), "open") {
		t.Fatalf("unexpected error: %v", err)
	}
}
