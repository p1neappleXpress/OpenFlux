package mobile

import (
	"sync"

	"github.com/p1neappleXpress/OpenFlux/transport/manager"
)

// initialCookies are cookies the app got before starting (a check passed
// with the tunnel down), applied to the Yandex carriers of the next start
// before their first request.
var initialCookies struct {
	mu  sync.Mutex
	jar map[string]string
}

// SetInitialCookies takes a Cookie header ("a=1; b=2") for the Yandex
// carriers (yandex, vyandex, boards) of the next Start*. "" clears it.
func SetInitialCookies(cookieHeader string) {
	jar := parseCookieHeader(cookieHeader)
	initialCookies.mu.Lock()
	initialCookies.jar = jar
	initialCookies.mu.Unlock()
}

func applyInitialCookies(m *manager.Manager, specs []sessionSpec) {
	initialCookies.mu.Lock()
	jar := initialCookies.jar
	initialCookies.mu.Unlock()
	if len(jar) == 0 {
		return
	}
	for _, s := range specs {
		switch s.Type {
		case "yandex", "vyandex", "boards":
			if err := m.AcceptCookies(s.Name, jar); err != nil {
				appendLog("[ANDROID] " + s.Name + ": начальные cookies не применились: " + err.Error())
			} else {
				appendLog("[ANDROID] " + s.Name + ": применены cookies, полученные до старта")
			}
		}
	}
}
