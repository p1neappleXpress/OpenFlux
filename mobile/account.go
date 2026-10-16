package mobile

import (
	"fmt"
	"strings"
	"sync"

	"github.com/p1neappleXpress/OpenFlux/transport/manager"
)

// clientSession is the running client Session's manager and its
// transports' types by name, so the app can hand the exit a sign-in.
var clientSession struct {
	mu    sync.Mutex
	m     *manager.Manager
	types map[string]string
}

func setClientSession(m *manager.Manager, types map[string]string) {
	clientSession.mu.Lock()
	clientSession.m, clientSession.types = m, types
	clientSession.mu.Unlock()
}

// OfferExitCookies sends a sign-in (a Cookie header, "a=1; b=2") to the
// exit for every transport of the client Session whose type is in
// transportTypes (comma-separated, e.g. "vyandex,yandex,boards"). The exit
// applies and keeps it. Returns how many transports it went to, or an
// error when no Session is connected.
func OfferExitCookies(transportTypes, cookieHeader string) (int, error) {
	jar := parseCookieHeader(cookieHeader)
	if len(jar) == 0 {
		return 0, fmt.Errorf("нет cookies")
	}
	wanted := map[string]bool{}
	for _, t := range strings.Split(transportTypes, ",") {
		if t = strings.TrimSpace(t); t != "" {
			wanted[t] = true
		}
	}
	clientSession.mu.Lock()
	m, types := clientSession.m, clientSession.types
	clientSession.mu.Unlock()
	if m == nil || !m.IsConnected() {
		return 0, fmt.Errorf("нет подключения к ноде")
	}
	sent := 0
	for name, typ := range types {
		if !wanted[typ] {
			continue
		}
		if err := m.OfferCookies(name, jar); err != nil {
			return sent, fmt.Errorf("%s: %w", name, err)
		}
		sent++
	}
	appendLog(fmt.Sprintf("[ANDROID] Вход передан ноде: %d транспорт(ов)", sent))
	return sent, nil
}
