package script

import (
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dop251/goja"
	"github.com/dop251/goja_nodejs/eventloop"

	"github.com/p1neappleXpress/OpenFlux/transport"
	"github.com/p1neappleXpress/OpenFlux/utils"
)

// EventHandler receives out-of-band events a script transport raises via
// raise(kind, payload) - captchaRequired, authRequired, needCookies, and
// whatever future kinds a transport author invents. Nothing in the host
// API restricts the set of kinds; this package just forwards them.
type EventHandler func(kind string, payload map[string]interface{})

// ScriptTransport runs one JS-defined transport inside its own goja
// runtime and event loop (one Runtime per transport goroutine - never
// touched from two goroutines at once). It implements transport.Transport
// so it drops into the existing factory/manager unchanged; every
// protocol detail (auth flow, wire framing, reconnect/keepalive policy)
// lives in the script, not here. Go's job is exactly three things: real
// I/O (HTTP, WebSocket, net.Dialer), the signature gate, and pumping
// callbacks between the script's loop and the rest of the core.
type ScriptTransport struct {
	*transport.BaseTransport

	name      string
	scriptSrc []byte
	pkg       *Package // non-nil when loaded from a .flux package, nil for a plain .js
	pubKey    ed25519.PublicKey // the key the package/.js was verified against; what an update must stay pinned to
	file      string            // base name it was loaded from, so an update replaces that very file
	cfgURL    string
	cfgParams map[string]interface{}

	httpClient           *http.Client
	httpClientNoRedirect *http.Client // http.fetch({redirect: "manual"})
	cookieJar            *cookiejar.Jar

	loop        *eventloop.EventLoop
	jsTransport *goja.Object
	info        Info

	writeCh chan []byte

	mu          sync.RWMutex
	lastState   string
	lastErr     string
	onEvent     EventHandler
	errNotifier func(err error, transportName, url, html, reason string)

	// roomList is the packed room list a script reported with
	// raise("roomList", {rooms}) (cupsonline: the rooms an exit keeps, what a
	// client needs as its url); onRoomList is told when it changes. Same
	// shape as the native cupsonline transport's RoomList/OnRoomList.
	roomMu     sync.Mutex
	roomList   string
	onRoomList func(packed string)

	// vm is the script's runtime, kept so Stop can interrupt JS that will
	// not return; closers close what the script opened (sockets, peer
	// connections) however it behaves.
	vm       *goja.Runtime
	closeMu  sync.Mutex
	closers  []func()
	stopOnce sync.Once

	// httpServers are this transport's own httpserver.listen() servers
	// (its setup/login mini-app - see host_httpserver.go), closed in Stop
	// so none outlives the transport.
	// pending are cookie values ApplyCookies was given before info() was read.
	pendingMu sync.Mutex
	pending   map[string]string
	infoReady bool

	httpServersMu sync.Mutex
	httpServers   []ownedServer
}

// New builds a script transport from a signed JS file at scriptPath. pubKey
// is the key the detached scriptPath+".sig" signature must verify against;
// loading fails closed if it doesn't (see sign.go). url/params map onto the
// same control.TransportConfig fields every other transport factory case
// already uses, just handed to the script's own open(cfg) instead of a Go
// constructor.
func New(name, scriptPath string, pubKey ed25519.PublicKey, url string, params map[string]interface{}, baseCfg transport.TransportConfig) (*ScriptTransport, error) {
	var src []byte
	var pkg *Package
	if strings.HasSuffix(scriptPath, ".flux") {
		p, err := LoadSignedPackage(scriptPath, pubKey)
		if err != nil {
			return nil, err
		}
		if p.Manifest.EffectiveAPI() > APIVersion {
			return nil, fmt.Errorf("script: %s needs host API %d, this core has %d", scriptPath, p.Manifest.EffectiveAPI(), APIVersion)
		}
		pkg, src = p, p.Script
	} else {
		s, err := LoadSigned(scriptPath, pubKey)
		if err != nil {
			return nil, err
		}
		src = s
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("script: cookie jar: %w", err)
	}

	t := &ScriptTransport{
		BaseTransport: transport.NewBaseTransport(baseCfg),
		name:          name,
		scriptSrc:     src,
		pkg:           pkg,
		pubKey:        append(ed25519.PublicKey(nil), pubKey...),
		file:          filepath.Base(scriptPath),
		cfgURL:        url,
		cfgParams:     params,
		cookieJar:     jar,
	}
	t.httpClient, t.httpClientNoRedirect = t.buildHTTPClients(jar)
	return t, nil
}

// RoomList is the packed list of rooms a script reported with
// raise("roomList", {rooms}); "" until it has.
func (t *ScriptTransport) RoomList() string {
	t.roomMu.Lock()
	defer t.roomMu.Unlock()
	return t.roomList
}

// OnRoomList calls f with the new list whenever RoomList changes.
func (t *ScriptTransport) OnRoomList(f func(packed string)) {
	t.roomMu.Lock()
	t.onRoomList = f
	t.roomMu.Unlock()
}

func (t *ScriptTransport) setRoomList(packed string) {
	t.roomMu.Lock()
	changed := packed != t.roomList
	t.roomList = packed
	f := t.onRoomList
	t.roomMu.Unlock()
	if changed && f != nil {
		f(packed)
	}
}

// Package returns the .flux package this transport was loaded from
// (author/description/icon for a UI listing), or nil for a plain signed .js
// file with no package wrapper.
func (t *ScriptTransport) Package() *Package { return t.pkg }

// Installed is the update record for this transport: what CheckUpdate /
// ApplyUpdate need to find, verify and pin a newer package. It is available
// only for a .flux loaded with an update source (ok is false for a plain .js,
// a package with no `update` URLs, or an empty pinned key - none of which can
// be updated in place), so a caller tests ok before enrolling it in a
// RecoveryLadder or an auto-update loop.
func (t *ScriptTransport) Installed() (inst Installed, ok bool) {
	if t.pkg == nil || len(t.pkg.Manifest.Update) == 0 || len(t.pubKey) == 0 {
		return Installed{}, false
	}
	m := t.pkg.Manifest
	return Installed{
		ID:        m.EffectiveID(),
		File:      t.file,
		Version:   m.Version,
		Wire:      m.EffectiveWire(),
		PubkeyHex: hex.EncodeToString(t.pubKey),
		Update:    append([]string(nil), m.Update...),
	}, true
}

// buildHTTPClients makes a follow/no-redirect client pair sharing one
// *http.Transport (so they share the connection pool and netbind-wrapped
// dialer) over the given jar. Used both for the script's default session
// (t.httpClient/httpClientNoRedirect) and for http.newSession() - a script
// managing several independent logical sessions on the same domain (e.g.
// cupsonline's 4 rooms, each with its own csrftoken) needs its own jar per
// session, since cookie names collide across sessions on one domain and a
// single shared jar would let one session's cookies silently clobber
// another's.
func (t *ScriptTransport) buildHTTPClients(jar *cookiejar.Jar) (*http.Client, *http.Client) {
	tr := &http.Transport{DialContext: t.dialer().DialContext}
	if t.info.HTTPMaxConnsPerHost > 0 {
		tr.MaxIdleConnsPerHost = t.info.HTTPMaxConnsPerHost
		tr.MaxConnsPerHost = t.info.HTTPMaxConnsPerHost
	}
	if t.info.HTTPMaxIdleConns > 0 {
		tr.MaxIdleConns = t.info.HTTPMaxIdleConns
	}
	if t.info.HTTPIdleConnTimeout > 0 {
		tr.IdleConnTimeout = t.info.HTTPIdleConnTimeout
	}
	client := &http.Client{Jar: jar, Timeout: 20 * time.Second, Transport: tr}
	noRedirect := &http.Client{
		Jar: jar, Timeout: 20 * time.Second, Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return client, noRedirect
}

// SetEventHandler registers the callback that receives OOB events the
// script raises. Extra method beyond transport.Transport, the same shape
// as mailru's CookieExchanger (FetchCookies/ApplyCookies) - callers that
// need it type-assert the concrete *ScriptTransport, everyone else just
// uses the plain transport.Transport interface.
func (t *ScriptTransport) SetEventHandler(h EventHandler) {
	t.mu.Lock()
	t.onEvent = h
	t.mu.Unlock()
}

func (t *ScriptTransport) eventHandler() EventHandler {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.onEvent
}

// SetErrorNotifier implements transport.ErrorNotifier: raise("captchaRequired",
// {url|html, reason}) (see js/template.js) reaches it through hostRaise,
// the same path every native transport's own captcha/login signal already
// takes - manager.go picks this up automatically, no script-specific
// wiring needed anywhere above this package.
func (t *ScriptTransport) SetErrorNotifier(fn func(err error, transportName, url, html, reason string)) {
	t.mu.Lock()
	t.errNotifier = fn
	t.mu.Unlock()
}

func (t *ScriptTransport) errorNotifier() func(err error, transportName, url, html, reason string) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.errNotifier
}

func (t *ScriptTransport) setLastState(state, err string) {
	t.mu.Lock()
	t.lastState, t.lastErr = state, err
	t.mu.Unlock()
}

// LastState returns the script's most recent setState() call, for
// diagnostics beyond the plain connected/disconnected bit IsConnected()
// exposes.
func (t *ScriptTransport) LastState() (state, errMsg string) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.lastState, t.lastErr
}

// Deliver pushes a downward OOB event (a captcha answer, applied cookies,
// an arbitrary inject) into the script via its optional onEvent(kind,
// payload) export. A script that doesn't define onEvent silently ignores
// it - Deliver is then a no-op, not an error.
func (t *ScriptTransport) Deliver(kind string, payload map[string]interface{}) error {
	loop := t.loop
	if loop == nil {
		return fmt.Errorf("script: transport not started")
	}
	ok := loop.RunOnLoop(func(vm *goja.Runtime) {
		fn, isFn := goja.AssertFunction(t.jsTransport.Get("onEvent"))
		if !isFn {
			return
		}
		p := vm.NewObject()
		for k, v := range payload {
			p.Set(k, v)
		}
		_, _ = fn(t.jsTransport, vm.ToValue(kind), p)
	})
	if !ok {
		return fmt.Errorf("script: transport loop terminated")
	}
	return nil
}

// Info returns the script's parsed manifest. Valid once Start() returns
// without error.
func (t *ScriptTransport) Info() Info { return t.info }

// tuneHTTPTransport applies info().http* pool sizing to the shared
// *http.Transport, once (bootstrap calls this right after parsing info(),
// before open() - and therefore before any request has gone through it,
// so mutating it in place is safe). A script that leaves these at 0 keeps
// Go's small default pool, same as before this field existed; a
// throughput-oriented script (concurrency.pool fan-out) sizes it to match.
func (t *ScriptTransport) tuneHTTPTransport() {
	tr, ok := t.httpClient.Transport.(*http.Transport)
	if !ok || tr == nil {
		return
	}
	if t.info.HTTPMaxConnsPerHost > 0 {
		tr.MaxIdleConnsPerHost = t.info.HTTPMaxConnsPerHost
		tr.MaxConnsPerHost = t.info.HTTPMaxConnsPerHost
	}
	if t.info.HTTPMaxIdleConns > 0 {
		tr.MaxIdleConns = t.info.HTTPMaxIdleConns
	}
	if t.info.HTTPIdleConnTimeout > 0 {
		tr.IdleConnTimeout = t.info.HTTPIdleConnTimeout
	}
}

// FetchCookies and ApplyCookies give every script transport the same
// cookie-exchange shape mailru/yandex expose natively (CookieExchanger),
// for free: Go and the script share one *cookiejar.Jar (host.go's
// cookieJar.get/set reads and writes the same jar), so reading the current
// cookies needs no round-trip into JS at all. Only forcing an already-open
// connection to pick up new cookies has to go through the script - only it
// knows how to tear down and reopen its own socket - so ApplyCookies
// delivers a "cookiesApplied" event a script may react to (see
// js/template.js); a script that ignores it just keeps using the old
// cookies until its own next reconnect.
func (t *ScriptTransport) FetchCookies() (map[string]string, error) {
	out := make(map[string]string)
	for _, c := range t.cookieJar.Cookies(t.cookieBaseURL()) {
		out[c.Name] = c.Value
	}
	return out, nil
}

func (t *ScriptTransport) ApplyCookies(values map[string]string) error {
	if len(values) == 0 {
		return nil
	}
	// Before the script has run, info().cookieDomain is not known: the values
	// would land under the default base address and the script would never see
	// them. (This is the replay of what an earlier run saved, which the
	// session and the apps do before Start.) Keep them until info() is read.
	t.pendingMu.Lock()
	if !t.infoReady {
		if t.pending == nil {
			t.pending = make(map[string]string, len(values))
		}
		for k, v := range values {
			t.pending[k] = v
		}
		t.pendingMu.Unlock()
		return nil
	}
	t.pendingMu.Unlock()
	t.writeCookies(values)
	return t.Deliver("cookiesApplied", nil)
}

// writeCookies puts values into the shared jar the way a browser would have
// from a Set-Cookie response header (see ScopeCookiesToParentDomain).
func (t *ScriptTransport) writeCookies(values map[string]string) {
	u := t.cookieBaseURL()
	domain := ""
	if t.info.ScopeCookiesToParentDomain {
		// Always end up with an EXPLICIT Domain (never "") so the cookie
		// domain-matches every subdomain, not just u's exact host: strip one
		// label if CookieDomain itself names a subdomain (docs.yandex.ru ->
		// yandex.ru), or keep it as-is if CookieDomain is already the apex
		// (yandex.js/vyandex.js both set "https://yandex.ru/" directly) -
		// either way domain ends up being the apex, explicitly. A host-only
		// cookie (Domain "") set against "yandex.ru" would never be sent on a
		// request to disk.yandex.ru/docs.yandex.ru, defeating the whole
		// point of this flag.
		if labels := strings.Split(u.Hostname(), "."); len(labels) >= 3 {
			domain = strings.Join(labels[1:], ".")
		} else {
			domain = u.Hostname()
		}
	}
	cookies := make([]*http.Cookie, 0, len(values))
	for k, v := range values {
		cookies = append(cookies, &http.Cookie{Name: k, Value: v, Path: "/", Domain: domain})
	}
	t.cookieJar.SetCookies(u, cookies)
}

// infoRead marks info() as parsed and puts in the values ApplyCookies kept
// until then. Called by bootstrap, before open().
func (t *ScriptTransport) infoRead() {
	t.pendingMu.Lock()
	t.infoReady = true
	pending := t.pending
	t.pending = nil
	t.pendingMu.Unlock()
	if len(pending) > 0 {
		t.writeCookies(pending)
	}
}

func (t *ScriptTransport) Start() error {
	if err := t.BaseTransport.Start(); err != nil {
		return err
	}
	t.writeCh = make(chan []byte, t.GetConfig().MaxQueueSize)
	t.loop = eventloop.NewEventLoop(eventloop.EnableConsole(true))

	// Queue the bootstrap job before Start(): RunOnLoop is documented safe
	// to call outside the loop, and Start()'s first act (loop.runAux())
	// drains exactly this queue before it does anything else.
	ready := make(chan error, 1)
	if !t.loop.RunOnLoop(func(vm *goja.Runtime) { ready <- t.bootstrap(vm) }) {
		return fmt.Errorf("script transport %s: loop already terminated", t.name)
	}

	// Start (not Run): Start()'s background goroutine holds jobCount at
	// >=1 for as long as it runs, so the loop stays alive for the
	// transport's whole lifetime regardless of the script's own timers.
	// Run() ties the loop's lifetime to the script's *own* pending
	// setTimeout/setInterval count instead, which hits zero (and silently
	// stops servicing RunOnLoop - writerPump, Deliver, http/ws callbacks -
	// with no error) the instant a script has no active timer, e.g. a
	// purely Promise-driven open() before its first setInterval runs.
	t.loop.Start()

	if err := <-ready; err != nil {
		t.loop.Terminate()
		return fmt.Errorf("script transport %s: %w", t.name, err)
	}

	utils.SafeGo("script."+t.name+".writer", t.writerPump)
	return nil
}

func (t *ScriptTransport) Stop() error {
	if err := t.BaseTransport.Stop(); err != nil {
		return err
	}
	t.stopOnce.Do(t.shutdown)
	return nil
}

// stopBudget is how long a script gets to wind down in close() before it is
// interrupted.
const stopBudget = 2 * time.Second

// shutdown asks the script to close (its own sockets, timers, state), then
// closes whatever it left open and ends the event loop. The script's close()
// was never called before: its WebSockets stayed open after Stop, so a
// participant stayed attached to the document - the native transports close
// their connection in Stop for exactly that reason.
func (t *ScriptTransport) shutdown() {
	if t.loop != nil && t.jsTransport != nil {
		done := make(chan struct{})
		posted := t.loop.RunOnLoop(func(vm *goja.Runtime) {
			defer close(done)
			if fn, ok := goja.AssertFunction(t.jsTransport.Get("close")); ok {
				_, _ = fn(t.jsTransport)
			}
		})
		if posted {
			select {
			case <-done:
			case <-time.After(stopBudget):
				// The loop is busy in JS that does not return (or close() hangs):
				// interrupt it, or the Terminate below would wait for ever.
				if t.vm != nil {
					t.vm.Interrupt("script transport stopped")
				}
			}
		}
	}
	t.closeHTTPServers()
	t.closeAll()
	if t.loop != nil {
		// Terminate (not Stop): also clears the script's keepalive/reconnect
		// timers, so nothing fires after this call returns.
		t.loop.Terminate()
	}
}

// addCloser registers something the host opened for the script; shutdown
// closes it even if the script's close() did not.
func (t *ScriptTransport) addCloser(f func()) {
	t.closeMu.Lock()
	t.closers = append(t.closers, f)
	t.closeMu.Unlock()
}

func (t *ScriptTransport) closeAll() {
	t.closeMu.Lock()
	fs := t.closers
	t.closers = nil
	t.closeMu.Unlock()
	for _, f := range fs {
		f()
	}
}

func (t *ScriptTransport) bootstrap(vm *goja.Runtime) error {
	t.vm = vm
	registerHostAPI(vm, t)
	if _, err := vm.RunString(string(t.scriptSrc)); err != nil {
		return fmt.Errorf("eval: %w", err)
	}
	obj, ok := vm.Get("Transport").(*goja.Object)
	if !ok || obj == nil {
		return fmt.Errorf("script must define a global `Transport` object")
	}
	t.jsTransport = obj

	infoFn, ok := goja.AssertFunction(obj.Get("info"))
	if !ok {
		return fmt.Errorf("Transport.info is not a function")
	}
	infoVal, err := infoFn(obj)
	if err != nil {
		return fmt.Errorf("Transport.info(): %w", err)
	}
	info, err := parseInfo(infoVal)
	if err != nil {
		return err
	}
	t.info = info
	t.infoRead()
	t.tuneHTTPTransport()

	openFn, ok := goja.AssertFunction(obj.Get("open"))
	if !ok {
		return fmt.Errorf("Transport.open is not a function")
	}
	cfg := vm.NewObject()
	params := vm.NewObject()
	// Every declared param gets a default for what the caller left unset (the
	// script need not repeat them in code, see Param.Default) - the profile
	// param included, so cfg.params[key] and cfg.url always agree: an app can
	// hand the profile value over either channel (or both) and the script
	// reads either the same way.
	resolved := WithDefaults(t.info.ResolvedParams(), t.cfgParams)
	cfgURL := t.cfgURL
	if cfgURL == "" {
		if pp := t.info.ProfileParam(); pp != nil {
			if v, ok := resolved[pp.Key].(string); ok {
				cfgURL = v
			}
		}
	}
	cfg.Set("url", cfgURL)
	for k, v := range resolved {
		params.Set(k, v)
	}
	cfg.Set("params", params)
	if _, err := openFn(obj, cfg); err != nil {
		return fmt.Errorf("Transport.open(): %w", err)
	}
	return nil
}

// Send enqueues one application packet for delivery. Non-blocking, same
// backpressure contract every native transport's Send() already has: a
// full queue is an error returned to the caller, never a stall.
func (t *ScriptTransport) Send(data []byte) error {
	if !t.IsConnected() {
		return fmt.Errorf("transport not connected")
	}
	select {
	case t.writeCh <- data:
		t.RecordSend(len(data))
		return nil
	default:
		return fmt.Errorf("write queue full")
	}
}

// writerPump drains writeCh and calls into the script's write() export on
// the script's own loop goroutine, one packet at a time. A packet that
// fails to send is retried after 15ms (same backoff mailru's native writer
// loop uses) rather than dropped - the reconnect the script schedules
// internally is what actually clears the jam.
func (t *ScriptTransport) writerPump() {
	var pending []byte
	for t.IsRunning() {
		if pending == nil {
			select {
			case p, ok := <-t.writeCh:
				if !ok {
					return
				}
				pending = p
			case <-t.Done():
				return
			}
		}

		result := make(chan bool, 1)
		posted := t.loop.RunOnLoop(func(vm *goja.Runtime) {
			writeFn, isFn := goja.AssertFunction(t.jsTransport.Get("write"))
			if !isFn {
				result <- false
				return
			}
			// pending is freshly dequeued and touched by nothing else, so
			// handing it to the script as an ArrayBuffer with no copy is
			// safe - this is the fast path the base64-string boundary used
			// to spend an encode on.
			buf := vm.NewArrayBuffer(pending)
			_, err := writeFn(t.jsTransport, vm.ToValue(buf))
			result <- err == nil
		})
		if !posted {
			return // loop already terminated
		}

		select {
		case ok := <-result:
			if ok {
				pending = nil
			} else {
				time.Sleep(15 * time.Millisecond)
			}
		case <-t.Done():
			return
		}
	}
}
