package registry

import (
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport/yandexcbir"
)

// slotPointers carries cbirIds through the Yandex suggest slots of a pool:
// one short token per chunk, no room, no chat limit. It is the channel the
// scripted transport uses, and it needs nothing but the pool both ends share.
type slotPointers struct {
	tr    *yandexcbir.Transport
	pool  *yandexcbir.Pool
	write []yandexcbir.Slot
	read  []yandexcbir.Slot
	http  *http.Client

	mu     sync.Mutex
	next   int
	closed bool
}

func newSlotPointers(tr *yandexcbir.Transport, pool *yandexcbir.Pool, isExit bool) *slotPointers {
	p := &slotPointers{tr: tr, pool: pool, http: &http.Client{Timeout: 30 * time.Second}}
	// The exit writes the s2c half and reads c2s; the client the other way
	// round, so the two never write the same slot.
	if isExit {
		p.write, p.read = pool.S2C, pool.C2S
	} else {
		p.write, p.read = pool.C2S, pool.S2C
	}
	return p
}

func (p *slotPointers) Send(id string) error {
	p.mu.Lock()
	if p.closed || len(p.write) == 0 {
		p.mu.Unlock()
		return nil
	}
	s := p.write[p.next%len(p.write)]
	p.next++
	p.mu.Unlock()
	go func() {
		if err := yandexcbir.Publish(p.http, s, id); err != nil {
			log.Printf("[cbir] slot publish failed: %v", err)
		}
	}()
	return nil
}

// run polls the read half and hands every fresh id to the transport.
func (p *slotPointers) run() {
	seen := map[string]bool{}
	for {
		time.Sleep(2 * time.Second)
		for _, s := range p.read {
			for _, id := range yandexcbir.Read(p.http, s) {
				if seen[id] {
					continue
				}
				seen[id] = true
				p.tr.DeliverPointer(id)
				log.Printf("[cbir] slot pointer in %s", id)
			}
		}
	}
}
