// Package yandexcbir carries OpenFlux packets through Yandex Images'
// search-by-image storage: each chunk of traffic rides in the payload chunk of
// a PNG, and the cbirId of that PNG travels over a pointer channel - the
// cups.online rooms - so the bulk never passes a chat surface.
package yandexcbir

import (
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport"
)

// newClient gives every upload its own TCP connection. With HTTP/2 all of them
// multiplexed on one connection and shared one flow-control window, which is
// why raising the upload count never raised the throughput.
func newClient() *http.Client {
	tr := &http.Transport{
		MaxIdleConns:        256,
		MaxIdleConnsPerHost: 64,
		MaxConnsPerHost:     32,
		IdleConnTimeout:     30 * time.Second,
		TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{},
		ForceAttemptHTTP2:   false,
	}
	return &http.Client{Timeout: 60 * time.Second, Transport: tr}
}

func envInt(key string) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 0
}

var dbg = func(format string, args ...any) {
	if os.Getenv("OPENFLUX_CBIR_DEBUG") != "" {
		log.Printf("[cbir] "+format, args...)
	}
}

const (
	// chunkBytes is how much traffic one image carries. 4 MiB is the measured
	// sweet spot: smaller pays the upload's fixed ~0.45 s per chunk.
	chunkBytes = 4 << 20
	// linger flushes a partly filled chunk so a small transfer is not stuck
	// waiting for the next 4 MiB.
	linger = 500 * time.Millisecond
	// ctrlBytes and ctrlLinger bound the control lane: small and often.
	ctrlBytes  = 8 << 10
	ctrlLinger = 30 * time.Millisecond
	// maxFrame keeps a single packet inside one chunk.
	maxFrame = 1 << 20
)

// PointerChannel is the low-latency bus the cbirIds travel over (cups rooms).
type PointerChannel interface {
	Send(id string) error
}

type Transport struct {
	client  *http.Client
	chunk   int
	sem     chan struct{}
	nextSeq atomic.Uint64

	selfMu  sync.Mutex
	selfIDs map[string]bool

	ordMu   sync.Mutex
	ordNext uint64
	ordBuf  map[uint64]string
	ordAt   atomic.Int64
	isExit  bool
	pool    *Pool

	sendCh chan []byte
	ptrCh  PointerChannel
	idsIn  chan string

	recvCb atomic.Value // func([]byte)
	closed atomic.Bool
	done   chan struct{}
	once   sync.Once

	bytesOut, bytesIn, pktsOut, pktsIn atomic.Uint64
	framesIn                           atomic.Uint64
	started                            time.Time
}

// New builds the carrier. url is the slot pool (kept for compatibility with
// configs that still name one); it is not needed when a pointer channel is
// set, because pointers then ride cups instead of a suggest slot.
func New(url string, isExit bool) *Transport {
	t := &Transport{
		client: newClient(),
		isExit: isExit,
		sendCh: make(chan []byte, 1024),
		done:   make(chan struct{}),
	}
	if url != "" {
		if p, err := ParsePool(url); err == nil {
			t.pool = p
		}
	}
	t.sem = make(chan struct{}, func() int {
		if n := envInt("OPENFLUX_CBIR_UPLOADS"); n > 0 && n <= 64 {
			return n
		}
		return 12
	}())
	if n := envInt("OPENFLUX_CBIR_CHUNK"); n >= 64<<10 && n <= 32<<20 {
		t.chunk = n
	}
	return t
}

// SetPointerChannel installs the bus for cbirIds. Without it the transport
// has no way to announce a chunk and stays idle.
func (t *Transport) SetPointerChannel(ch PointerChannel) {
	t.ptrCh = ch
	if t.idsIn == nil {
		t.idsIn = make(chan string, 1024)
	}
}

// DeliverControl hands a small frame (an ack, a control packet) to the session.
func (t *Transport) DeliverControl(frame []byte) {
	if cb, ok := t.recvCb.Load().(func([]byte)); ok && cb != nil {
		t.bytesIn.Add(uint64(len(frame)))
		t.pktsIn.Add(1)
		cb(frame)
	}
}

// DeliverPointer is called by the pointer channel for every id it receives.
func (t *Transport) DeliverPointer(id string) {
	// An entry may carry "seq:id". Chunks then reach the session in publish
	// order, which is what keeps the tunnel's TCP from seeing gaps and
	// retransmitting: retransmissions were ~40% of the uploaded bytes.
	// Ordering is opt-in: when it was on by default a single skipped sequence
	// number swallowed the whole receive path and the peer never answered.
	if os.Getenv("OPENFLUX_CBIR_ORDER") != "1" {
		if i := strings.IndexByte(id, ':'); i > 0 {
			t.pushID(id[i+1:])
			return
		}
	}
	if i := strings.IndexByte(id, ':'); i > 0 {
		if seq, err := strconv.ParseUint(id[:i], 10, 64); err == nil {
			t.deliverOrdered(seq, id[i+1:])
			return
		}
	}
	t.pushID(id)
}

func (t *Transport) noteSelf(id string) {
	t.selfMu.Lock()
	if t.selfIDs == nil {
		t.selfIDs = make(map[string]bool, 256)
	}
	t.selfIDs[id] = true
	t.selfMu.Unlock()
}

func (t *Transport) isSelf(id string) bool {
	t.selfMu.Lock()
	ok := t.selfIDs[id]
	t.selfMu.Unlock()
	return ok
}

func (t *Transport) pushID(id string) {
	// The tag is derived from the key, so both ends share it and cannot use it
	// to tell their own messages apart. The ids we minted are the real answer.
	if t.isSelf(id) {
		return
	}
	if t.idsIn == nil {
		return
	}
	select {
	case t.idsIn <- id:
	default:
	}
}

// deliverOrdered hands ids to the downloader strictly in sequence. Anything
// that arrives early waits; if the gap does not close in time the buffer is
// flushed so one lost pointer cannot stall the stream.
func (t *Transport) deliverOrdered(seq uint64, id string) {
	t.ordMu.Lock()
	// A restarted peer numbers its chunks from zero again: catch that and
	// start over, or the old counter swallows every new id.
	if seq == 0 || seq+1000 < t.ordNext {
		t.ordNext = 0
		t.ordBuf = nil
	}
	if seq < t.ordNext {
		t.ordMu.Unlock()
		return
	}
	if t.ordBuf == nil {
		t.ordBuf = make(map[uint64]string, 64)
	}
	t.ordBuf[seq] = id
	var ready []string
	for {
		v, ok := t.ordBuf[t.ordNext]
		if !ok {
			break
		}
		delete(t.ordBuf, t.ordNext)
		ready = append(ready, v)
		t.ordNext++
	}
	t.ordAt.Store(time.Now().UnixNano())
	stuck := len(t.ordBuf)
	t.ordMu.Unlock()
	for _, v := range ready {
		t.pushID(v)
	}
	if stuck > 64 {
		t.ordMu.Lock()
		for k, v := range t.ordBuf {
			if k < t.ordNext+1024 {
				continue
			}
			delete(t.ordBuf, k)
			t.pushID(v)
		}
		t.ordMu.Unlock()
	}
}

func (t *Transport) Start() error {
	if t.ptrCh == nil {
		return errors.New("yandexcbir: no pointer channel (cups rooms are required)")
	}
	t.started = time.Now()
	go t.sendLoop()
	go t.recvLoop()
	go t.orderWatchdog()
	return nil
}

func (t *Transport) Stop() error {
	t.once.Do(func() { close(t.done) })
	t.closed.Store(true)
	return nil
}

func (t *Transport) IsConnected() bool { return !t.closed.Load() }

func (t *Transport) Send(data []byte) error {
	if len(data) > maxFrame {
		return errors.New("yandexcbir: frame too large")
	}
	pkt := make([]byte, len(data))
	copy(pkt, data)
	select {
	case t.sendCh <- pkt:
		return nil
	case <-t.done:
		return errors.New("yandexcbir: closed")
	}
}

func (t *Transport) Receive(callback func([]byte)) { t.recvCb.Store(callback) }

func (t *Transport) Stats() transport.TransportStats {
	return transport.TransportStats{
		BytesSent:     t.bytesOut.Load(),
		BytesReceived: t.bytesIn.Load(),
		PacketsSent:   t.pktsOut.Load(),
		PacketsRecv:   t.pktsIn.Load(),
		Connected:     t.IsConnected(),
		Uptime:        time.Since(t.started),
	}
}

// sendLoop packs frames into one chunk and publishes its cbirId.
func (t *Transport) sendLoop() {
	var pending []byte
	var ctrl []byte
	ctrlAt := time.Now()
	timer := time.NewTimer(linger)
	defer timer.Stop()
	flush := func() {
		if len(pending) == 0 {
			return
		}
		payload := pending
		pending = nil
		go func() {
			t.sem <- struct{}{}
			defer func() { <-t.sem }()
			id, err := upload(t.client, payload)
			if err != nil {
				dbg("upload failed: %v", err)
				return
			}
			t.noteSelf(id)
			if err := t.ptrCh.Send(id); err != nil {
				dbg("announce %s failed: %v", id, err)
				return
			}
			dbg("uploaded %d bytes as %s", len(payload), id)
			t.bytesOut.Add(uint64(len(payload)))
			t.pktsOut.Add(1)
		}()
	}
	for {
		select {
		case <-t.done:
			flush()
			return
		case pkt := <-t.sendCh:
			// Control frames ride their own small chunk, flushed every few
			// milliseconds: the peer's TCP measures its round trip by them, and
			// a round trip that includes a megabyte upload kept cwnd at 18.
			if len(pkt) <= 1500 && t.ptrCh != nil {
				var cl [4]byte
				binary.BigEndian.PutUint32(cl[:], uint32(len(pkt)))
				ctrl = append(ctrl, cl[:]...)
				ctrl = append(ctrl, pkt...)
				if len(ctrl) >= ctrlBytes || time.Since(ctrlAt) >= ctrlLinger {
					t.flushCtrl(ctrl)
					ctrl = nil
					ctrlAt = time.Now()
				}
				continue
			}
			var l [4]byte
			binary.BigEndian.PutUint32(l[:], uint32(len(pkt)))
			pending = append(pending, l[:]...)
			pending = append(pending, pkt...)
			limit := chunkBytes
			if t.chunk > 0 {
				limit = t.chunk
			}
			if len(pending) >= limit {
				flush()
			}
		case <-timer.C:
			flush()
			if len(ctrl) > 0 {
				t.flushCtrl(ctrl)
				ctrl = nil
				ctrlAt = time.Now()
			}
			timer.Reset(linger)
		}
	}
}

// flushCtrl ships the control buffer as its own small chunk.
func (t *Transport) flushCtrl(frame []byte) {
	payload := append([]byte(nil), frame...)
	go func() {
		t.sem <- struct{}{}
		defer func() { <-t.sem }()
		id, err := upload(t.client, payload)
		if err != nil {
			dbg("ctrl upload failed: %v", err)
			return
		}
		dbg("uploaded ctrl %d bytes as %s", len(payload), id)
		seq := t.nextSeq.Add(1) - 1
		if t.ptrCh != nil {
			_ = t.ptrCh.Send(fmt.Sprintf("%d:%s", seq, id))
		}
	}()
}

// recvLoop turns announced cbirIds into frames.
// orderWatchdog skips a sequence number that never arrives: one lost pointer
// must not freeze the stream, which is exactly what happened under load.
func (t *Transport) orderWatchdog() {
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-t.done:
			return
		case <-tick.C:
			last := t.ordAt.Load()
			if last != 0 && time.Since(time.Unix(0, last)) < 2*time.Second {
				continue
			}
			t.ordMu.Lock()
			if len(t.ordBuf) == 0 {
				t.ordMu.Unlock()
				continue
			}
			var low uint64
			first := true
			for k := range t.ordBuf {
				if first || k < low {
					low, first = k, false
				}
			}
			t.ordNext = low
			var ready []string
			for {
				v, ok := t.ordBuf[t.ordNext]
				if !ok {
					break
				}
				delete(t.ordBuf, t.ordNext)
				ready = append(ready, v)
				t.ordNext++
			}
			t.ordAt.Store(time.Now().UnixNano())
			t.ordMu.Unlock()
			for _, v := range ready {
				t.pushID(v)
			}
		}
	}
}

func (t *Transport) recvLoop() {
	for {
		select {
		case <-t.done:
			return
		case id := <-t.idsIn:
			go func(id string) {
				payload, err := download(t.client, id)
				if err != nil {
					dbg("download %s failed: %v", id, err)
					return
				}
				dbg("downloaded %d bytes for %s", len(payload), id)
				n := t.framesIn.Add(1)
				if n%20 == 0 {
					dbg("frames into session: %d", n)
				}
				cb, _ := t.recvCb.Load().(func([]byte))
				for len(payload) >= 4 {
					n := int(binary.BigEndian.Uint32(payload[:4]))
					if n < 0 || len(payload) < 4+n {
						break
					}
					frame := payload[4 : 4+n]
					payload = payload[4+n:]
					if cb != nil {
						cb(frame)
					}
					t.bytesIn.Add(uint64(n))
					t.pktsIn.Add(1)
				}
			}(id)
		}
	}
}
