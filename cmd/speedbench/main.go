// speedbench measures the real direct-transport stack end to end: one process
// listens (server, plays the exit), the other dials (client). Nothing touches
// the host network except the one TCP connection.
//
//	speedbench -mode=server -addr=0.0.0.0:47301 -stack=full -key-file=k
//	speedbench -mode=client -addr=host:47301    -stack=full -key-file=k -mb=64 -pkt=1300
//
// Stacks (each adds one layer on top of the previous):
//
//	tcp    plain net.Conn, 64 KiB writes: the path capacity (ceiling)
//	direct transport.DirectTransport only (2-byte record framing)
//	enc    EncryptedTransport(DirectTransport)
//	full   BatchedTransport(EncryptedTransport(DirectTransport)): what a Session link is
//
// Phases (client driven): connect time, latency (ping-pong of small packets),
// upload (client -> server), download (server -> client), duplex (both at once),
// small (many tiny packets), conns (N parallel connections, goodput total).
package main

import (
	"bufio"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"os"
	"runtime/pprof"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport"
)

const (
	kData  = 'D'
	kEcho  = 'E' // request an echo
	kReply = 'R'
	kDown  = 'S' // server: start sending N bytes of pkt-size packets
	kEnd   = 'Z' // end of a one-way stream; payload = total bytes sent
	kAck   = 'A' // payload: bytes(8) ns(8) received by the peer for that stream
)

type conn interface {
	Send([]byte) error
	Recv() <-chan []byte
	Close()
	Connected() bool
	Stats() transport.TransportStats
}

func main() {
	mode := flag.String("mode", "client", "server | client")
	addr := flag.String("addr", "127.0.0.1:47301", "listen (server) or dial (client) address")
	stack := flag.String("stack", "full", "tcp | direct | enc | full")
	keyFile := flag.String("key-file", "", "shared secret file (enc, full)")
	mb := flag.Int("mb", 64, "MB per throughput phase")
	pkt := flag.Int("pkt", 1300, "tunnel packet size in bytes")
	pings := flag.Int("pings", 200, "ping-pong samples")
	phases := flag.String("phases", "connect,latency,up,down,duplex,small,conns", "comma list")
	nconns := flag.Int("conns", 8, "parallel connections for the conns phase")
	cpuprof := flag.String("cpuprofile", "", "write a CPU profile here")
	label := flag.String("label", "", "label printed with the results")
	to := flag.String("to", "", "delay mode: target host:port")
	delayMs := flag.Float64("delay-ms", 35, "delay mode: one-way delay in ms")
	rateKB := flag.Int("rate-kb", 0, "delay mode: rate limit KB/s per direction (0 = none)")
	queueKB := flag.Int("queue-kb", 0, "delay mode: bound the in-flight queue (backpressure like a real bottleneck), 0 = unbounded")
	stallHzF := flag.Float64("stall-hz", 0, "delay mode: HOL stalls per second per direction")
	stallMs := flag.Float64("stall-ms", 90, "delay mode: stall duration in ms")
	flag.Parse()

	var secret string
	if *stack == "enc" || *stack == "full" {
		b, err := os.ReadFile(*keyFile)
		if err != nil {
			log.Fatalf("key-file: %v", err)
		}
		secret = strings.TrimSpace(string(b))
	}
	if *cpuprof != "" {
		f, err := os.Create(*cpuprof)
		if err != nil {
			log.Fatal(err)
		}
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}

	stallHz, stallDur = *stallHzF, time.Duration(*stallMs*float64(time.Millisecond))
	if *queueKB > 0 {
		queueChunks = max(1, *queueKB/16)
	}
	switch *mode {
	case "delay":
		runDelayProxy(*addr, *to, time.Duration(*delayMs*float64(time.Millisecond)), *rateKB*1000)
		return
	case "http":
		runHTTP(*addr)
		return
	}
	if *mode == "server" {
		serve(*addr, *stack, secret)
		return
	}
	runClient(*addr, *stack, secret, *mb, *pkt, *pings, strings.Split(*phases, ","), *nconns, *label)
}

// ---- stacks ----

type tcpConn struct {
	c    net.Conn
	rx   chan []byte
	once sync.Once
	rb   atomic.Uint64
	wb   atomic.Uint64
	wmu  sync.Mutex
}

func newTCP(c net.Conn) *tcpConn {
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	t := &tcpConn{c: c, rx: make(chan []byte, 4096)}
	go func() {
		br := bufio.NewReaderSize(c, 256<<10)
		var h [2]byte
		for {
			if _, err := io.ReadFull(br, h[:]); err != nil {
				close(t.rx)
				return
			}
			n := int(binary.BigEndian.Uint16(h[:]))
			p := make([]byte, n)
			if _, err := io.ReadFull(br, p); err != nil {
				close(t.rx)
				return
			}
			t.rb.Add(uint64(n))
			t.rx <- p
		}
	}()
	return t
}
func (t *tcpConn) Send(p []byte) error {
	buf := make([]byte, 2+len(p))
	binary.BigEndian.PutUint16(buf, uint16(len(p)))
	copy(buf[2:], p)
	t.wmu.Lock()
	_, err := t.c.Write(buf)
	t.wmu.Unlock()
	t.wb.Add(uint64(len(p)))
	return err
}
func (t *tcpConn) Recv() <-chan []byte { return t.rx }
func (t *tcpConn) Close()              { t.c.Close() }
func (t *tcpConn) Connected() bool     { return true }
func (t *tcpConn) Stats() transport.TransportStats {
	return transport.TransportStats{BytesSent: t.wb.Load(), BytesReceived: t.rb.Load()}
}

type trConn struct {
	tr transport.Transport
	rx chan []byte
}

func newTransportConn(stack string, secret string, exit bool, addr string) (*trConn, error) {
	cfg := transport.DefaultConfig()
	dc := transport.DefaultDirectConfig()
	dc.IsExit = exit
	if exit {
		dc.ListenAddr = addr
	} else {
		dc.DialAddr = addr
	}
	var tr transport.Transport = transport.NewDirectTransport(cfg, dc)
	if stack == "enc" || stack == "full" {
		e, err := transport.NewEncryptedTransport(tr, secret, "speedbench", exit)
		if err != nil {
			return nil, err
		}
		tr = e
	}
	if stack == "full" {
		tr = transport.NewBatchedTransport(tr)
	}
	t := &trConn{tr: tr, rx: make(chan []byte, 8192)}
	tr.Receive(func(p []byte) {
		// The callback buffer is only valid for the call in some layers.
		cp := make([]byte, len(p))
		copy(cp, p)
		t.rx <- cp
	})
	if err := tr.Start(); err != nil {
		return nil, err
	}
	return t, nil
}
func (t *trConn) Send(p []byte) error {
	for {
		if err := t.tr.Send(p); err == nil {
			return nil
		}
		time.Sleep(200 * time.Microsecond) // queue full: backpressure
	}
}
func (t *trConn) Recv() <-chan []byte { return t.rx }
func (t *trConn) Close()              { t.tr.Stop() }
func (t *trConn) Connected() bool     { return t.tr.IsConnected() }
func (t *trConn) Stats() transport.TransportStats {
	return t.tr.Stats()
}

// ---- server ----

func serve(addr, stack, secret string) {
	log.Printf("speedbench server stack=%s on %s", stack, addr)
	if stack == "tcp" {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			log.Fatal(err)
		}
		for {
			c, err := ln.Accept()
			if err != nil {
				log.Fatal(err)
			}
			go handle(newTCP(c))
		}
	}
	// DirectTransport accepts one peer at a time and keeps listening, so
	// the server is one long-lived transport that serves successive clients.
	t, err := newTransportConn(stack, secret, true, addr)
	if err != nil {
		log.Fatal(err)
	}
	handle(t)
	select {}
}

func handle(c conn) {
	var got uint64
	var first, last time.Time
	for p := range c.Recv() {
		if len(p) == 0 {
			continue
		}
		switch p[0] {
		case kData:
			if got == 0 {
				first = time.Now()
			}
			got += uint64(len(p))
			last = time.Now()
		case kEcho:
			p[0] = kReply
			c.Send(p)
		case kDown:
			n := binary.BigEndian.Uint64(p[1:9])
			size := int(binary.BigEndian.Uint32(p[9:13]))
			go sendStream(c, n, size)
		case kEnd:
			var ack [17]byte
			ack[0] = kAck
			binary.BigEndian.PutUint64(ack[1:], got)
			binary.BigEndian.PutUint64(ack[9:], uint64(last.Sub(first)))
			c.Send(ack[:])
			got = 0
		}
	}
}

func sendStream(c conn, total uint64, size int) uint64 {
	buf := make([]byte, size)
	var sent uint64
	for sent < total {
		rand.Read(buf)
		buf[0] = kData
		if err := c.Send(buf); err != nil {
			break
		}
		sent += uint64(size)
	}
	var end [9]byte
	end[0] = kEnd
	binary.BigEndian.PutUint64(end[1:], sent)
	c.Send(end[:])
	return sent
}

// ---- client ----

type result struct {
	name string
	val  string
}

func runClient(addr, stack, secret string, mb, pkt, pings int, phases []string, nconns int, label string) {
	want := map[string]bool{}
	for _, p := range phases {
		want[p] = true
	}
	var out []result
	add := func(n, f string, a ...interface{}) { out = append(out, result{n, fmt.Sprintf(f, a...)}) }
	cpu0 := cpuTime()
	wall0 := time.Now()

	dial := func() (*peer, time.Duration, error) {
		t0 := time.Now()
		if stack == "tcp" {
			c, err := net.DialTimeout("tcp", addr, 15*time.Second)
			if err != nil {
				return nil, 0, err
			}
			return newPeer(newTCP(c)), time.Since(t0), nil
		}
		t, err := newTransportConn(stack, secret, false, addr)
		if err != nil {
			return nil, 0, err
		}
		for !t.Connected() {
			if time.Since(t0) > 30*time.Second {
				return nil, 0, fmt.Errorf("not connected after 30s")
			}
			time.Sleep(time.Millisecond)
		}
		return newPeer(t), time.Since(t0), nil
	}

	c, ct, err := dial()
	if err != nil {
		log.Fatalf("dial: %v", err)
	}
	defer c.Close()
	// First echo round trip proves the whole path is up (handshake incl.).
	t0 := time.Now()
	echo(c, firstEcho)
	add("connect_ms", "%.1f (tcp+start) / %.1f (first echo ok)", ms(ct), ms(ct+time.Since(t0)))

	if want["latency"] {
		var rtts []float64
		for i := 0; i < pings; i++ {
			t := time.Now()
			echo(c, echoSize)
			rtts = append(rtts, ms(time.Since(t)))
		}
		sort.Float64s(rtts)
		mean, jit := meanJitter(rtts)
		add("latency_ms", "p50=%.1f p90=%.1f p99=%.1f min=%.1f max=%.1f mean=%.1f jitter(sd)=%.1f n=%d",
			rtts[len(rtts)/2], rtts[len(rtts)*9/10], rtts[len(rtts)*99/100], rtts[0], rtts[len(rtts)-1], mean, jit, len(rtts))
	}

	total := uint64(mb) << 20
	if want["up"] {
		sent, el, ackBytes, ackNs := upload(c, total, pkt)
		add("upload", "%.2f MB/s (%.1f Mbit/s) recv-side %.2f MB/s | sent %.1f MB in %.2fs",
			mbps(ackBytes, ackNs), mbps(ackBytes, ackNs)*8, mbps(ackBytes, ackNs), float64(sent)/1e6, el.Seconds())
	}
	if want["down"] {
		got, el := download(c, total, pkt)
		add("download", "%.2f MB/s (%.1f Mbit/s) | %.1f MB in %.2fs", mbps(got, int64(el)), mbps(got, int64(el))*8, float64(got)/1e6, el.Seconds())
	}
	if want["duplex"] {
		var wg sync.WaitGroup
		var upMB, downMB float64
		t := time.Now()
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _, b, ns := upload(c, total, pkt)
			upMB = mbps(b, ns)
		}()
		go func() {
			defer wg.Done()
			g, el := download(c, total, pkt)
			downMB = mbps(g, int64(el))
		}()
		wg.Wait()
		add("duplex", "up %.2f MB/s + down %.2f MB/s = %.2f MB/s total in %.2fs", upMB, downMB, upMB+downMB, time.Since(t).Seconds())
	}
	if want["small"] {
		// 20000 tiny packets one way; measures per-packet overhead and
		// batching rather than bandwidth.
		const n = 20000
		small := make([]byte, 100)
		rand.Read(small)
		small[0] = kData
		t := time.Now()
		for i := 0; i < n; i++ {
			c.Send(small)
		}
		var end [9]byte
		end[0] = kEnd
		c.Send(end[:])
		a := waitAck(c, 120*time.Second)
		el := time.Since(t)
		got := binary.BigEndian.Uint64(a[1:])
		add("small_pkts", "%.0f pkt/s (%.2f MB/s) | %d x 100 B in %.2fs | server got %.1f%%",
			float64(n)/el.Seconds(), float64(got)/1e6/el.Seconds(), n, el.Seconds(), 100*float64(got)/float64(n*100))
	}
	if want["conns"] && stack == "tcp" {
		c.Close()
		var wg sync.WaitGroup
		var totalBytes atomic.Uint64
		t := time.Now()
		var fails atomic.Int32
		per := total / uint64(nconns)
		for i := 0; i < nconns; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				cc, _, err := dial()
				if err != nil {
					fails.Add(1)
					return
				}
				defer cc.Close()
				_, _, b, _ := upload(cc, per, pkt)
				totalBytes.Add(b)
			}()
		}
		wg.Wait()
		el := time.Since(t)
		add("conns", "%d parallel: %.2f MB/s total (%d failed) in %.2fs", nconns, float64(totalBytes.Load())/1e6/el.Seconds(), fails.Load(), el.Seconds())
	}

	cpu := cpuTime() - cpu0
	st := c.Stats()
	fmt.Printf("== speedbench %s stack=%s pkt=%d mb=%d addr=%s ==\n", label, stack, pkt, mb, addr)
	for _, r := range out {
		fmt.Printf("  %-12s %s\n", r.name, r.val)
	}
	fmt.Printf("  %-12s %.2fs cpu in %.2fs wall (client process) | chan msgs sent=%d recv=%d\n", "cpu", cpu.Seconds(), time.Since(wall0).Seconds(), st.PacketsSent, st.PacketsRecv)
}

// peer routes everything the remote sends so phases can run concurrently.
type peer struct {
	conn
	data    atomic.Uint64
	firstNs atomic.Int64
	endCh   chan uint64
	ackCh   chan []byte
	replyCh chan struct{}
}

func newPeer(c conn) *peer {
	p := &peer{conn: c, endCh: make(chan uint64, 16), ackCh: make(chan []byte, 16), replyCh: make(chan struct{}, 1024)}
	go func() {
		for b := range c.Recv() {
			if len(b) == 0 {
				continue
			}
			if os.Getenv("SB_TRACE") != "" {
				log.Printf("trace: rx %d bytes kind=%q", len(b), b[0])
			}
			switch b[0] {
			case kData:
				p.firstNs.CompareAndSwap(0, time.Now().UnixNano())
				p.data.Add(uint64(len(b)))
			case kEnd:
				p.endCh <- binary.BigEndian.Uint64(b[1:])
			case kAck:
				p.ackCh <- b
			case kReply:
				p.replyCh <- struct{}{}
			}
		}
	}()
	return p
}

func echo(c *peer, size int) {
	p := make([]byte, size)
	rand.Read(p)
	p[0] = kEcho
	c.Send(p)
	select {
	case <-c.replyCh:
	case <-time.After(30 * time.Second):
		log.Fatalf("echo timeout")
	}
}

func waitAck(c *peer, timeout time.Duration) []byte {
	select {
	case a := <-c.ackCh:
		return a
	case <-time.After(timeout):
		log.Fatalf("ack timeout")
	}
	return nil
}

func upload(c *peer, total uint64, pkt int) (sent uint64, el time.Duration, ackBytes uint64, ackNs int64) {
	buf := make([]byte, pkt)
	t := time.Now()
	for sent < total {
		rand.Read(buf)
		buf[0] = kData
		c.Send(buf)
		sent += uint64(pkt)
	}
	var end [9]byte
	end[0] = kEnd
	binary.BigEndian.PutUint64(end[1:], sent)
	c.Send(end[:])
	a := waitAck(c, 300*time.Second)
	el = time.Since(t)
	return sent, el, binary.BigEndian.Uint64(a[1:]), int64(binary.BigEndian.Uint64(a[9:]))
}

// download asks the server for total bytes; throughput is counted from the
// first data packet to the server's end marker.
func download(c *peer, total uint64, pkt int) (got uint64, el time.Duration) {
	base := c.data.Load()
	c.firstNs.Store(0)
	var req [13]byte
	req[0] = kDown
	binary.BigEndian.PutUint64(req[1:], total)
	binary.BigEndian.PutUint32(req[9:], uint32(pkt))
	c.Send(req[:])
	select {
	case <-c.endCh:
	case <-time.After(300 * time.Second):
		log.Fatalf("download timeout")
	}
	first := c.firstNs.Load()
	el = time.Since(time.Unix(0, first))
	return c.data.Load() - base, el
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func mbps(bytes uint64, ns int64) float64 {
	if ns <= 0 {
		return 0
	}
	return float64(bytes) / 1e6 / (float64(ns) / 1e9)
}

func meanJitter(v []float64) (mean, sd float64) {
	for _, x := range v {
		mean += x
	}
	mean /= float64(len(v))
	for _, x := range v {
		sd += (x - mean) * (x - mean)
	}
	return mean, sqrt(sd / float64(len(v)))
}

func sqrt(x float64) float64 {
	z := x / 2
	if z == 0 {
		return 0
	}
	for i := 0; i < 30; i++ {
		z = (z + x/z) / 2
	}
	return z
}

// Echo payload sizes: the first echo proves the path, the others sample latency.
var (
	firstEcho = 32
	echoSize  = 64
)

func init() {
	if v := os.Getenv("SB_ECHO"); v != "" {
		fmt.Sscanf(v, "%d", &echoSize)
		firstEcho = echoSize
	}
}
