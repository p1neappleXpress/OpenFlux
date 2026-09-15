//go:build android

package main

/*
#include <stdlib.h>
*/
import "C"

import (
	"fmt"
	"openflux-android-tun/bridge"
	"sync"
)

var registry = struct {
	sync.Mutex
	next      uint64
	sessions  map[uint64]*bridge.Session
	lastError string
}{sessions: make(map[uint64]*bridge.Session)}

func session(h C.ulonglong) *bridge.Session {
	registry.Lock()
	defer registry.Unlock()
	return registry.sessions[uint64(h)]
}

//export OpenFluxTunCreate
func OpenFluxTunCreate(fd C.int, port C.int, mtu C.int) C.ulonglong {
	s, err := bridge.New(int(fd), int(port), int(mtu))
	registry.Lock()
	defer registry.Unlock()
	if err != nil {
		registry.lastError = err.Error()
		return 0
	}
	registry.next++
	registry.sessions[registry.next] = s
	registry.lastError = ""
	return C.ulonglong(registry.next)
}

//export OpenFluxTunRun
func OpenFluxTunRun(h C.ulonglong) C.int {
	s := session(h)
	if s == nil {
		return -1
	}
	if err := s.Run(); err != nil {
		registry.Lock()
		registry.lastError = fmt.Sprint(err)
		registry.Unlock()
		return -1
	}
	return 0
}

//export OpenFluxTunStop
func OpenFluxTunStop(h C.ulonglong) {
	if s := session(h); s != nil {
		s.Stop()
	}
}

//export OpenFluxTunDestroy
func OpenFluxTunDestroy(h C.ulonglong) {
	registry.Lock()
	s := registry.sessions[uint64(h)]
	delete(registry.sessions, uint64(h))
	registry.Unlock()
	if s != nil {
		s.Stop()
	}
}

//export OpenFluxTunStats
func OpenFluxTunStats(h C.ulonglong) *C.char {
	if s := session(h); s != nil {
		return C.CString(s.Stats())
	}
	return C.CString("{}")
}

//export OpenFluxTunError
func OpenFluxTunError() *C.char {
	registry.Lock()
	defer registry.Unlock()
	return C.CString(registry.lastError)
}

func main() {}
