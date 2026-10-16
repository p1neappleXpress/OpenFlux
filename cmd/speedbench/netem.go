package main

import (
	"crypto/rand"
	"io"
	"log"
	mrand "math/rand"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// runDelayProxy relays TCP connections adding a fixed one-way delay in both
// directions (in order, no loss) and an optional per-direction rate limit in
// bytes/s: a poor man's WAN for repeatable local runs.
var (
	queueChunks = 1 << 14
	stallHz     float64 // head-of-line stalls per second per direction (emulates carrier TCP loss recovery)
	stallDur    time.Duration
)

func runDelayProxy(listen, target string, delay time.Duration, rate int) {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("delay proxy %s -> %s one-way %v rate %d B/s", listen, target, delay, rate)
	for {
		c, err := ln.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go func() {
			t, err := net.Dial("tcp", target)
			if err != nil {
				c.Close()
				return
			}
			for _, x := range []net.Conn{c, t} {
				if tc, ok := x.(*net.TCPConn); ok {
					tc.SetNoDelay(true)
				}
			}
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); pipeDelayed(t, c, delay, rate); t.Close() }()
			go func() { defer wg.Done(); pipeDelayed(c, t, delay, rate); c.Close() }()
			wg.Wait()
		}()
	}
}

type delayed struct {
	at time.Time
	b  []byte
}

func pipeDelayed(dst, src net.Conn, delay time.Duration, rate int) {
	q := make(chan delayed, queueChunks) // reader -> pacer (bounded: backpressure like a bottleneck)
	q2 := make(chan delayed, 1<<14)      // pacer -> stall gate
	done := make(chan struct{})
	var stallUntil atomic.Int64
	if stallHz > 0 {
		go func() {
			for {
				select {
				case <-done:
					return
				default:
				}
				time.Sleep(time.Duration(mrand.ExpFloat64() / stallHz * float64(time.Second)))
				stallUntil.Store(time.Now().Add(stallDur).UnixNano())
			}
		}()
	}
	// Stage 1: one-way delay and the bottleneck rate.
	go func() {
		defer close(q2)
		var next time.Time
		for d := range q {
			if w := time.Until(d.at); w > 0 {
				time.Sleep(w)
			}
			if rate > 0 {
				now := time.Now()
				if next.After(now) {
					time.Sleep(next.Sub(now))
				}
				next = time.Now().Add(time.Duration(float64(len(d.b)) / float64(rate) * float64(time.Second)))
			}
			q2 <- d
		}
	}()
	// Stage 2: head-of-line stalls hold delivery but do not cost capacity; what
	// piled up behind a stall is released at once, as a TCP receiver does when
	// a retransmission fills the hole.
	go func() {
		defer close(done)
		for d := range q2 {
			if w := time.Until(time.Unix(0, stallUntil.Load())); w > 0 {
				time.Sleep(w)
			}
			if _, err := dst.Write(d.b); err != nil {
				for range q2 {
				}
				return
			}
		}
	}()
	buf := make([]byte, 16<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			q <- delayed{time.Now().Add(delay), append([]byte(nil), buf[:n]...)}
		}
		if err != nil {
			break
		}
	}
	close(q)
	<-done
}

// runHTTP serves /big (random bytes, ?mb=N, default 256), /small and POST /up.
func runHTTP(addr string) {
	chunk := make([]byte, 1<<20)
	rand.Read(chunk)
	mux := http.NewServeMux()
	mux.HandleFunc("/big", func(w http.ResponseWriter, r *http.Request) {
		mb := 256
		if v, err := strconv.Atoi(r.URL.Query().Get("mb")); err == nil && v > 0 {
			mb = v
		}
		w.Header().Set("Content-Length", strconv.Itoa(mb<<20))
		for i := 0; i < mb; i++ {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	})
	mux.HandleFunc("/up", func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/small", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	log.Printf("http on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
