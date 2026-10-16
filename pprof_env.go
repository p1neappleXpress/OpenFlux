package main

import (
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
)

// OPENFLUX_PPROF=127.0.0.1:6060 serves Go's pprof endpoints for profiling a
// running core (CPU, heap, goroutines). Off unless the variable is set; bind it
// to loopback.
func init() {
	addr := os.Getenv("OPENFLUX_PPROF")
	if addr == "" {
		return
	}
	go func() {
		log.Printf("pprof listening on %s", addr)
		if err := http.ListenAndServe(addr, nil); err != nil {
			log.Printf("pprof: %v", err)
		}
	}()
}
