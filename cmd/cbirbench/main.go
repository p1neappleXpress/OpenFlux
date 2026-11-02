package main

// cbirbench measures how fast this host can push chunks into Yandex Images:
// the tunnel's ceiling is exactly this number divided by the overlap factor.

import (
	"crypto/rand"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport/yandexcbir"
)

func main() {
	par, _ := strconv.Atoi(os.Getenv("PAR"))
	if par == 0 {
		par = 12
	}
	size, _ := strconv.Atoi(os.Getenv("SIZE"))
	if size == 0 {
		size = 192 << 10
	}
	secs, _ := strconv.Atoi(os.Getenv("SECS"))
	if secs == 0 {
		secs = 15
	}
	var sent, ok, failed atomic.Int64
	deadline := time.Now().Add(time.Duration(secs) * time.Second)
	var wg sync.WaitGroup
	for i := 0; i < par; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, size)
			_, _ = rand.Read(buf)
			for time.Now().Before(deadline) {
				if _, err := yandexcbir.UploadForTest(buf); err != nil {
					failed.Add(1)
					continue
				}
				ok.Add(1)
				sent.Add(int64(size))
			}
		}()
	}
	wg.Wait()
	mb := float64(sent.Load()) / (1 << 20)
	fmt.Printf("par=%d size=%d ok=%d failed=%d bytes=%.1fMB rate=%.2f MB/s\n",
		par, size, ok.Load(), failed.Load(), mb, mb/float64(secs))
}
