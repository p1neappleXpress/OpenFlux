package streamproxy

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport/phpbox"
)

// loopCarrier wires the client mux to a tiny in-process phpbox exit.
type loopCarrier struct {
	recv func([]byte)
	mu   sync.Mutex
	rx   []byte
	conn map[uint32]net.Conn
}

func (c *loopCarrier) Receive(cb func([]byte)) { c.recv = cb }
func (c *loopCarrier) Start() error            { c.conn = map[uint32]net.Conn{}; return nil }
func (c *loopCarrier) Stop() error             { return nil }
func (c *loopCarrier) emit(f phpbox.Frame)     { c.recv(phpbox.Encode(nil, f)) }
func (c *loopCarrier) Send(b []byte) error {
	c.mu.Lock()
	c.rx = append(c.rx, b...)
	var fs []phpbox.Frame
	for {
		f, rest, ok := phpbox.TakeFrame(c.rx)
		if !ok {
			break
		}
		c.rx = rest
		fs = append(fs, f)
	}
	c.mu.Unlock()
	for _, f := range fs {
		switch f.Type {
		case phpbox.FrameOpen:
			addr := strings.SplitN(string(f.Payload), "\x00", 2)[0]
			go func(f phpbox.Frame) {
				conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
				if err != nil {
					c.emit(phpbox.Frame{Type: phpbox.FrameOpenErr, StreamID: f.StreamID, Payload: []byte("refused")})
					return
				}
				c.mu.Lock()
				c.conn[f.StreamID] = conn
				c.mu.Unlock()
				c.emit(phpbox.Frame{Type: phpbox.FrameOpenOK, StreamID: f.StreamID})
				buf := make([]byte, 32768)
				for {
					n, err := conn.Read(buf)
					if n > 0 {
						c.emit(phpbox.Frame{Type: phpbox.FrameData, StreamID: f.StreamID, Payload: append([]byte(nil), buf[:n]...)})
					}
					if err != nil {
						c.emit(phpbox.Frame{Type: phpbox.FrameClose, StreamID: f.StreamID})
						return
					}
				}
			}(f)
		case phpbox.FrameData:
			c.mu.Lock()
			conn := c.conn[f.StreamID]
			c.mu.Unlock()
			if conn != nil {
				conn.Write(f.Payload)
			}
		case phpbox.FrameClose:
			c.mu.Lock()
			conn := c.conn[f.StreamID]
			delete(c.conn, f.StreamID)
			c.mu.Unlock()
			if conn != nil {
				conn.Close()
			}
		}
	}
	return nil
}

func freeAddr(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// A SOCKS5 client and an HTTP client both reach a destination through the proxy.
func TestProxyServesSocks5AndHTTP(t *testing.T) {
	srv := http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "hello via stream") })}
	dst, _ := net.Listen("tcp", "127.0.0.1:0")
	go srv.Serve(dst)
	defer srv.Close()

	socks, httpAddr := freeAddr(t), freeAddr(t)
	p, err := Start(Options{Carrier: &loopCarrier{}, Socks: socks, HTTP: httpAddr})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Stop()
	if !p.Connected() {
		t.Fatal("a carrier that cannot say must count as connected")
	}

	// SOCKS5 CONNECT by hand (no auth).
	c, err := net.DialTimeout("tcp", socks, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte{5, 1, 0})
	r := bufio.NewReader(c)
	if b, _ := r.Peek(2); len(b) < 2 {
		t.Fatal("no method reply")
	}
	r.Discard(2)
	host, portStr, _ := net.SplitHostPort(dst.Addr().String())
	var port int
	for _, ch := range portStr {
		port = port*10 + int(ch-'0')
	}
	req := append([]byte{5, 1, 0, 1}, net.ParseIP(host).To4()...)
	req = append(req, byte(port>>8), byte(port))
	c.Write(req)
	reply := make([]byte, 10)
	if _, err := io.ReadFull(r, reply); err != nil || reply[1] != 0 {
		t.Fatalf("CONNECT reply %v err %v", reply, err)
	}
	c.Write([]byte("GET / HTTP/1.0\r\n\r\n"))
	body, _ := io.ReadAll(r)
	if !strings.Contains(string(body), "hello via stream") {
		t.Fatalf("through SOCKS5 got %q", body)
	}

	// The HTTP proxy.
	hc := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: func(*http.Request) (*url.URL, error) { return url.Parse("http://" + httpAddr) }}}
	resp, err := hc.Get("http://" + dst.Addr().String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "hello via stream" {
		t.Fatalf("through the HTTP proxy got %q", b)
	}
	// The relay counts a direction when its copy ends, which can be a moment after the client has its answer.
	deadline := time.Now().Add(3 * time.Second)
	for (p.BytesReceived() == 0 || p.BytesSent() == 0) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if p.BytesReceived() == 0 || p.BytesSent() == 0 {
		t.Errorf("counters did not move: sent %d received %d", p.BytesSent(), p.BytesReceived())
	}
}

func TestStartRejectsBadOptions(t *testing.T) {
	if _, err := Start(Options{Socks: "127.0.0.1:0"}); err == nil {
		t.Error("no carrier must be an error")
	}
	if _, err := Start(Options{Carrier: &loopCarrier{}}); err == nil {
		t.Error("no address must be an error")
	}
	busy, _ := net.Listen("tcp", "127.0.0.1:0")
	defer busy.Close()
	if _, err := Start(Options{Carrier: &loopCarrier{}, Socks: busy.Addr().String()}); err == nil {
		t.Error("a taken port must be an error")
	}
}
