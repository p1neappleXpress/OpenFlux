package main

import (
	"bytes"
	"log"
	mrand "math/rand"
	"sync/atomic"
	"time"

	"universal-bypass-tool/transport"
)

// A distinct end-of-stream marker the sink recognizes. 41 bytes; a random
// payload matching it is astronomically unlikely.
var benchSentinel = []byte("==OPENFLUX-BENCH-END==_padding_0123456789")

const benchPktSize = 1300

func mbps(nBytes, ns int64) float64 {
	if ns <= 0 {
		return 0
	}
	return (float64(nBytes) / 1e6) / (float64(ns) / 1e9)
}

func ratio(a, b int64) float64 {
	if b <= 0 {
		return 0
	}
	return float64(a) / float64(b)
}

// runBenchSink receives packets and measures delivered goodput plus the
// channel-message count (which exposes the batching factor).
func runBenchSink(trans transport.Transport) {
	var got, pkts, startNs int64
	done := make(chan int64, 1)

	trans.Receive(func(p []byte) {
		if bytes.Equal(p, benchSentinel) {
			s := atomic.LoadInt64(&startNs)
			if s == 0 {
				s = time.Now().UnixNano()
			}
			select {
			case done <- time.Now().UnixNano() - s:
			default:
			}
			return
		}
		if atomic.CompareAndSwapInt64(&startNs, 0, time.Now().UnixNano()) {
			log.Printf("[BENCH-SINK] first packet received, timing started")
		}
		atomic.AddInt64(&got, int64(len(p)))
		atomic.AddInt64(&pkts, 1)
	})

	if err := trans.Start(); err != nil {
		log.Fatalf("[BENCH-SINK] start: %v", err)
	}
	log.Printf("[BENCH-SINK] waiting for sender...")

	go func() {
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		for range tick.C {
			s := atomic.LoadInt64(&startNs)
			if s == 0 {
				continue
			}
			el := time.Now().UnixNano() - s
			g := atomic.LoadInt64(&got)
			st := trans.Stats()
			log.Printf("[BENCH-SINK] %.2f MB, %.3f MB/s | tunnel pkts=%d | channel msgs=%d (%.1f pkts/msg)",
				float64(g)/1e6, mbps(g, el), atomic.LoadInt64(&pkts),
				st.PacketsRecv, ratio(atomic.LoadInt64(&pkts), int64(st.PacketsRecv)))
		}
	}()

	el := <-done
	g := atomic.LoadInt64(&got)
	st := trans.Stats()
	log.Printf("==========================================================")
	log.Printf("[BENCH-SINK] DONE")
	log.Printf("  delivered    : %.2f MB", float64(g)/1e6)
	log.Printf("  elapsed      : %.2f s", float64(el)/1e9)
	log.Printf("  GOODPUT      : %.3f MB/s (%.0f KB/s)", mbps(g, el), mbps(g, el)*1000)
	log.Printf("  tunnel pkts  : %d", atomic.LoadInt64(&pkts))
	log.Printf("  channel msgs : %d", st.PacketsRecv)
	log.Printf("  batching     : %.1f tunnel pkts per channel msg", ratio(atomic.LoadInt64(&pkts), int64(st.PacketsRecv)))
	log.Printf("  channel bytes: %.2f MB (pre-base64, on-wire is ~1.33x)", float64(st.BytesReceived)/1e6)
	log.Printf("==========================================================")
}

// runBenchSend pushes mb megabytes through the transport as ~MTU-sized packets,
// honoring queue backpressure, then reports the channel-message count.
func runBenchSend(trans transport.Transport, mb int, compressible bool) {
	if err := trans.Start(); err != nil {
		log.Fatalf("[BENCH-SEND] start: %v", err)
	}
	log.Printf("[BENCH-SEND] connecting...")
	for i := 0; i < 600 && !trans.IsConnected(); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if !trans.IsConnected() {
		log.Fatalf("[BENCH-SEND] transport not connected after 60s")
	}
	log.Printf("[BENCH-SEND] connected; pushing %d MB (compressible=%v)", mb, compressible)

	total := int64(mb) * 1024 * 1024
	payload := make([]byte, benchPktSize)
	if compressible {
		for i := range payload {
			payload[i] = byte('A' + (i % 16))
		}
	}

	var sent int64
	start := time.Now()
	for sent < total {
		if compressible {
			payload[0] = byte(sent)
			payload[1] = byte(sent >> 8)
		} else {
			mrand.Read(payload)
		}
		for trans.Send(payload) != nil {
			time.Sleep(time.Millisecond) // queue full: natural backpressure
		}
		sent += benchPktSize
	}
	for trans.Send(benchSentinel) != nil {
		time.Sleep(time.Millisecond)
	}

	el := time.Since(start)
	st := trans.Stats()
	tunnelPkts := sent / benchPktSize
	log.Printf("[BENCH-SEND] enqueued %.2f MB in %.2fs", float64(sent)/1e6, el.Seconds())
	log.Printf("[BENCH-SEND] channel msgs=%d, %.1f tunnel pkts/msg, channel bytes=%.2f MB",
		st.PacketsSent, ratio(tunnelPkts, int64(st.PacketsSent)), float64(st.BytesSent)/1e6)
	log.Printf("[BENCH-SEND] draining to channel...")
	time.Sleep(3 * time.Second)
	st = trans.Stats()
	log.Printf("[BENCH-SEND] after drain: channel msgs=%d channel bytes=%.2f MB", st.PacketsSent, float64(st.BytesSent)/1e6)
}
