package mobile

import (
	"strings"
	"sync"

	"github.com/p1neappleXpress/OpenFlux/transport"
)

// route tracks which carrier the running connection uses, for the UI.
var route struct {
	mu      sync.Mutex
	session *transport.Session
	types   map[string]string // Session transport name -> type
	classic string            // type in classic single-transport mode
}

func setSessionRoute(s *transport.Session, types map[string]string) {
	route.mu.Lock()
	route.session, route.types, route.classic = s, types, ""
	route.mu.Unlock()
}

func setClassicRoute(transportType string) {
	route.mu.Lock()
	route.session, route.types, route.classic = nil, nil, transportType
	route.mu.Unlock()
}

func clearRoute() { setClassicRoute("") }

// CurrentTransport returns the type of the carrier traffic currently goes
// through ("direct", "yandex", ...), or "" when nothing is connected. In
// Session mode it follows failover between carriers.
func CurrentTransport() string {
	route.mu.Lock()
	s, types, classic := route.session, route.types, route.classic
	route.mu.Unlock()
	if s != nil {
		name := s.ActiveTransport()
		if t := types[name]; t != "" {
			return t
		}
		return name
	}
	if classic != "" && (IsConnected() || ProxyIsConnected() || ExitIsConnected()) {
		return classic
	}
	return ""
}

// CurrentTransports names every carrier traffic currently goes through,
// joined by ",": several in Session mode when they share the highest
// priority. Session carriers go by name ("boards", "boards-2"), a classic
// connection by its type; "" when nothing is connected.
func CurrentTransports() string {
	route.mu.Lock()
	s, classic := route.session, route.classic
	route.mu.Unlock()
	if s != nil {
		return strings.Join(s.ActiveTransports(), ",")
	}
	if classic != "" && (IsConnected() || ProxyIsConnected() || ExitIsConnected()) {
		return classic
	}
	return ""
}

// ConnectionMode says how traffic currently goes: "session" (the
// authenticated Session), "classic" (the exit does not answer the Session
// handshake: an older or classic node, or it has not answered yet), or ""
// (not connected / no key).
func ConnectionMode() string {
	route.mu.Lock()
	s := route.session
	route.mu.Unlock()
	if s == nil {
		return ""
	}
	return s.Mode()
}
