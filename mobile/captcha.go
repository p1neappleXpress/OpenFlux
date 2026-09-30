package mobile

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport"
	"github.com/p1neappleXpress/OpenFlux/transport/manager"
)

// Android side of the core's out-of-band captcha/login flow. Desktop and iOS
// route it over transport/ipc; gomobile calls straight in instead: the app
// polls PendingCaptchaURL, lets the user pass the check in a WebView, and
// hands the resulting cookies to SubmitCaptchaCookies.
var captcha struct {
	mu     sync.Mutex
	store  *transport.CookieStore
	key    string
	url    string
	reason string
	// apply hands cookies to the live transport that asked for them; nil
	// once that transport stopped or failed to start.
	apply func(map[string]string) error

	// proxy is set when the check belongs to the exit (remoteName is its
	// transport): the page must be opened through this HTTP proxy so it
	// is passed from the exit's address, and the cookies go to the exit.
	proxy      string
	remoteName string
	// snooze holds off repeated exit checks the user cancelled.
	snooze map[string]time.Time
}

// remoteSnooze is how long a cancelled exit check stays quiet; the exit
// repeats its report every 20-30s while stuck.
const remoteSnooze = 10 * time.Minute

// SetCookieStorePath enables persisting solved-captcha cookies across app
// restarts. Call once before Start/StartProxy; returns "" or an error.
func SetCookieStorePath(path string) string {
	store, err := transport.NewCookieStore(path)
	if err != nil {
		return err.Error()
	}
	captcha.mu.Lock()
	captcha.store = store
	captcha.mu.Unlock()
	return ""
}

// attachCaptcha wires the raw (unwrapped, not yet started) transport into
// the flow and replays cookies saved from an earlier solve.
func attachCaptcha(transportType, documentURL string, raw transport.Transport) {
	captcha.mu.Lock()
	defer captcha.mu.Unlock()
	captcha.key = transportType + " " + documentURL
	captcha.url = ""
	captcha.reason = ""
	captcha.proxy, captcha.remoteName = "", ""
	captcha.apply = nil
	exchanger, _ := raw.(transport.CookieExchanger)
	if exchanger != nil {
		captcha.apply = exchanger.ApplyCookies
	}
	if notifier, ok := raw.(transport.ErrorNotifier); ok {
		notifier.SetErrorNotifier(func(err error, name, url, reason string) {
			appendLog(fmt.Sprintf("[ANDROID] %s: нужна проверка в браузере (%s)", name, reason))
			captcha.mu.Lock()
			captcha.url = url
			captcha.reason = reason
			captcha.mu.Unlock()
		})
	}
	if captcha.store != nil && exchanger != nil {
		if saved := captcha.store.Load(captcha.key); len(saved) > 0 {
			_ = exchanger.ApplyCookies(saved)
		}
	}
}

// attachSessionCaptcha does the same for a Session: the Manager reports
// which transport needs the check, and keys maps each cookie-carrying
// transport to its store key. Saved cookies are replayed by the Manager.
func attachSessionCaptcha(m *manager.Manager, keys map[string]string, proxy *authProxy) {
	captcha.mu.Lock()
	captcha.key, captcha.url, captcha.reason, captcha.apply = "", "", "", nil
	captcha.proxy, captcha.remoteName = "", ""
	store := captcha.store
	captcha.mu.Unlock()
	if store != nil {
		for name, key := range keys {
			if err := m.UseCookieStore(store, name, key); err != nil {
				appendLog(fmt.Sprintf("[ANDROID] %s: сохранённые cookies не применились: %v", name, err))
			}
		}
	}
	m.SetCaptchaNotifier(func(name, url, reason string) {
		appendLog(fmt.Sprintf("[ANDROID] %s: нужна проверка в браузере (%s)", name, reason))
		captcha.mu.Lock()
		captcha.url, captcha.reason, captcha.key = url, reason, keys[name]
		captcha.proxy, captcha.remoteName = "", ""
		captcha.apply = func(jar map[string]string) error { return m.ApplyCookiesFor(name, jar) }
		captcha.mu.Unlock()
	})
	if proxy == nil {
		return
	}
	// A check the exit's own transport hit (AuthRequired): it has to be
	// passed from the exit's address, so the page goes through the tunnel,
	// and the cookies go back to the exit, which applies and keeps them.
	m.SetRemoteAuthNotifier(func(name, url, reason string) {
		captcha.mu.Lock()
		quiet := captcha.url != "" || time.Now().Before(captcha.snooze[name])
		captcha.mu.Unlock()
		if quiet {
			return
		}
		addr, err := proxy.Addr()
		if err != nil || addr == "" {
			appendLog(fmt.Sprintf("[ERROR] Прокси для проверки ноды: %v", err))
			return
		}
		appendLog(fmt.Sprintf("[ANDROID] Нода: %s требует проверку в браузере (%s)", name, reason))
		captcha.mu.Lock()
		captcha.url, captcha.reason, captcha.key = url, reason, ""
		captcha.proxy, captcha.remoteName = addr, name
		captcha.apply = func(jar map[string]string) error { return m.OfferCookies(name, jar) }
		captcha.mu.Unlock()
	})
}

// PendingCaptchaProxy returns the HTTP proxy (host:port) to open the
// pending page through when the check belongs to the exit, or "" when it
// is the phone's own.
func PendingCaptchaProxy() string {
	captcha.mu.Lock()
	defer captcha.mu.Unlock()
	return captcha.proxy
}

// detachCaptcha drops the live transport reference, so cookies submitted
// afterwards are only saved for the next start, never applied to a
// transport that failed to start or was stopped.
func detachCaptcha() {
	captcha.mu.Lock()
	captcha.apply = nil
	captcha.mu.Unlock()
}

// PendingCaptchaURL returns the page the user must open to pass a captcha
// or log in, or "" when nothing is pending.
func PendingCaptchaURL() string {
	captcha.mu.Lock()
	defer captcha.mu.Unlock()
	return captcha.url
}

// PendingCaptchaReason is "smartcaptcha" or "login" while a check is pending.
func PendingCaptchaReason() string {
	captcha.mu.Lock()
	defer captcha.mu.Unlock()
	return captcha.reason
}

// CancelCaptcha clears the pending check without cookies (user gave up).
// An exit check stays quiet for a while instead of popping up again.
func CancelCaptcha() {
	captcha.mu.Lock()
	if captcha.remoteName != "" {
		if captcha.snooze == nil {
			captcha.snooze = make(map[string]time.Time)
		}
		captcha.snooze[captcha.remoteName] = time.Now().Add(remoteSnooze)
	}
	captcha.url = ""
	captcha.reason = ""
	captcha.proxy, captcha.remoteName = "", ""
	captcha.mu.Unlock()
}

// SubmitCaptchaCookies takes a Cookie header ("a=1; b=2", the format of
// Android's CookieManager.getCookie), saves it for future starts and applies
// it to the running transport. Returns "" or a user-readable error.
func SubmitCaptchaCookies(cookieHeader string) string {
	jar := parseCookieHeader(cookieHeader)
	if len(jar) == 0 {
		return "Cookies не получены"
	}
	captcha.mu.Lock()
	store, key, apply := captcha.store, captcha.key, captcha.apply
	captcha.url = ""
	captcha.reason = ""
	captcha.proxy, captcha.remoteName = "", ""
	captcha.mu.Unlock()

	if store != nil && key != "" {
		if err := store.Save(key, jar); err != nil {
			appendLog(fmt.Sprintf("[ERROR] Не удалось сохранить cookies: %v", err))
		}
	}
	if apply != nil {
		// ApplyCookies may sleep through a reconnect backoff.
		go func() {
			if err := apply(jar); err != nil {
				appendLog(fmt.Sprintf("[ERROR] Применение cookies: %v", err))
			}
		}()
	}
	appendLog(fmt.Sprintf("[ANDROID] Получено cookies: %d", len(jar)))
	return ""
}

func parseCookieHeader(header string) map[string]string {
	jar := make(map[string]string)
	for _, part := range strings.Split(header, ";") {
		name, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && name != "" {
			jar[name] = value
		}
	}
	return jar
}
