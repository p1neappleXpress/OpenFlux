package script

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"github.com/dop251/goja"
	"github.com/gorilla/websocket"
	"github.com/pierrec/lz4/v4"

	"github.com/p1neappleXpress/OpenFlux/netbind"
	"github.com/p1neappleXpress/OpenFlux/utils"
)

// registerHostAPI injects the host surface into a freshly created runtime.
// Everything here is deliberately unrestricted - no capability scoping, no
// allowlisted hosts - the only gate a script passes through is the
// signature check in sign.go, done once before this ever runs. console.*
// is registered separately by the eventloop package itself.
func registerHostAPI(vm *goja.Runtime, t *ScriptTransport) {
	registerHTTP(vm, t)
	registerWS(vm, t)
	registerCookieJar(vm, t)
	registerBase64(vm)
	registerText(vm)
	registerURL(vm)
	registerGzip(vm)
	registerLZ4(vm)
	registerCrypto(vm)
	registerConcurrency(vm, t)
	registerUDP(vm, t)
	registerWebRTC(vm, t)
	registerHTTPServer(vm, t)

	vm.Set("emit", hostEmit(vm, t))
	vm.Set("setState", hostSetState(t))
	vm.Set("raise", hostRaise(vm, t))
}

// ---- text.decode / text.encode ----
//
// goja has no TextDecoder/TextEncoder. Needed wherever a script pulls JSON
// or other text out of a bytes-like value (a base64- or gzip-decoded
// blob) - JSON.parse takes a string, not an ArrayBuffer.
func registerText(vm *goja.Runtime) {
	textObj := vm.NewObject()
	textObj.Set("decode", func(call goja.FunctionCall) goja.Value {
		b, err := bytesArg(vm, call.Argument(0))
		if err != nil {
			panic(vm.NewTypeError(err.Error()))
		}
		return vm.ToValue(string(b))
	})
	textObj.Set("encode", func(call goja.FunctionCall) goja.Value {
		return vm.ToValue(vm.NewArrayBuffer([]byte(call.Argument(0).String())))
	})
	vm.Set("text", textObj)
}

// ---- url.parse ----
//
// goja has no WHATWG URL global, and a hand-rolled regex parse of every
// redirect Location a script follows is exactly the kind of fragile string
// surgery scripting was supposed to get away from - so it's a host
// primitive backed by Go's own net/url, not a JS reimplementation.
func registerURL(vm *goja.Runtime) {
	urlObj := vm.NewObject()
	urlObj.Set("parse", func(call goja.FunctionCall) goja.Value {
		u, err := url.Parse(call.Argument(0).String())
		if err != nil {
			panic(vm.NewTypeError(err.Error()))
		}
		out := vm.NewObject()
		out.Set("href", u.String())
		out.Set("protocol", u.Scheme+":")
		out.Set("hostname", u.Hostname())
		out.Set("host", u.Host)
		out.Set("pathname", u.Path)
		out.Set("search", u.RawQuery)
		out.Set("hash", u.Fragment)
		return out
	})
	vm.Set("url", urlObj)
}

// ---- gzip.compress / gzip.decompress ----
//
// Generic codec, same rationale as base64: a script whose wire format
// happens to need gzip (e.g. a captcha fingerprint blob) uses the host's
// implementation instead of writing one in JS.
func registerGzip(vm *goja.Runtime) {
	gz := vm.NewObject()
	gz.Set("compress", func(call goja.FunctionCall) goja.Value {
		b, err := bytesArg(vm, call.Argument(0))
		if err != nil {
			panic(vm.NewTypeError(err.Error()))
		}
		var buf bytes.Buffer
		w := gzip.NewWriter(&buf)
		if _, err := w.Write(b); err != nil {
			panic(vm.NewTypeError(err.Error()))
		}
		if err := w.Close(); err != nil {
			panic(vm.NewTypeError(err.Error()))
		}
		return vm.ToValue(vm.NewArrayBuffer(buf.Bytes()))
	})
	gz.Set("decompress", func(call goja.FunctionCall) goja.Value {
		b, err := bytesArg(vm, call.Argument(0))
		if err != nil {
			panic(vm.NewTypeError(err.Error()))
		}
		r, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			panic(vm.NewTypeError(err.Error()))
		}
		out, err := io.ReadAll(r)
		if err != nil {
			panic(vm.NewTypeError(err.Error()))
		}
		return vm.ToValue(vm.NewArrayBuffer(out))
	})
	vm.Set("gzip", gz)
}

// ---- lz4.decompressBlock ----
//
// oneme's incoming-call payload (the "vcp" field) is an LZ4 BLOCK (not the
// gzip/streaming format gzip.decompress handles) - github.com/pierrec/lz4/v4
// is already a dependency via transport/oneme's native implementation, no
// new module. Block format needs the decompressed size upfront (there's no
// end-of-stream marker), so this takes it explicitly rather than guessing.
func registerLZ4(vm *goja.Runtime) {
	l := vm.NewObject()
	l.Set("decompressBlock", func(call goja.FunctionCall) goja.Value {
		b, err := bytesArg(vm, call.Argument(0))
		if err != nil {
			panic(vm.NewTypeError(err.Error()))
		}
		size := int(call.Argument(1).ToInteger())
		dst := make([]byte, size)
		n, err := lz4.UncompressBlock(b, dst)
		if err != nil {
			panic(vm.NewGoError(err))
		}
		return vm.ToValue(vm.NewArrayBuffer(dst[:n]))
	})
	vm.Set("lz4", l)
}

// ---- crypto.sha256 / crypto.solvePow ----
func registerCrypto(vm *goja.Runtime) {
	c := vm.NewObject()
	c.Set("sha256", func(call goja.FunctionCall) goja.Value {
		b, err := bytesArg(vm, call.Argument(0))
		if err != nil {
			panic(vm.NewTypeError(err.Error()))
		}
		sum := sha256.Sum256(b)
		return vm.ToValue(vm.NewArrayBuffer(sum[:]))
	})
	// solvePow is the one deliberate exception to "everything lives in the
	// script": a hash-based proof-of-work challenge (Yandex SmartCaptcha's
	// fast path) needs millions of SHA-256 attempts at realistic
	// complexity, and paying a Go<->JS call per attempt would dwarf the
	// cost of the hash itself - the same "keep the hot loop native"
	// tradeoff that keeps `direct` a native transport. Everything AROUND
	// the PoW (fetching the challenge page, parsing it, building the
	// fingerprint, POSTing the answer) still lives entirely in the script.
	// prefix mirrors the native solver's own fallback: it's normally hex,
	// but if it doesn't decode as hex the raw string bytes are used as-is.
	c.Set("solvePow", func(call goja.FunctionCall) goja.Value {
		prefixStr := call.Argument(0).String()
		complexity := int(call.Argument(1).ToInteger())
		prefix, err := hex.DecodeString(prefixStr)
		if err != nil || len(prefix) == 0 {
			prefix = []byte(prefixStr)
		}
		nonceHex, attempts := solvePow(prefix, complexity)
		out := vm.NewObject()
		out.Set("nonceHex", nonceHex)
		out.Set("attempts", attempts)
		return out
	})
	vm.Set("crypto", c)
}

// solvePow brute-forces a 16-byte nonce (8 bytes millisecond timestamp + 8
// bytes random) until sha256(nonce||prefix) has `complexity` leading zero
// bits. Literal port of yandex/captcha.go's solveCaptchaPoW/
// captchaCheckComplexity.
func solvePow(prefix []byte, complexity int) (string, int) {
	var nonce [16]byte
	for attempts := 1; attempts < 10_000_000; attempts++ {
		binary.LittleEndian.PutUint64(nonce[0:8], uint64(time.Now().UnixMilli()))
		binary.LittleEndian.PutUint64(nonce[8:16], uint64(rand.Int63()))
		h := sha256.New()
		h.Write(nonce[:])
		h.Write(prefix)
		sum := h.Sum(nil)
		if powComplexityOK(sum, complexity) {
			return hex.EncodeToString(nonce[:]), attempts
		}
	}
	return "", 0
}

func powComplexityOK(h []byte, complexity int) bool {
	if complexity < 0 || complexity > 8*len(h) {
		return false
	}
	e, o := 0, 0
	for e <= complexity-8 {
		if h[o] != 0 {
			return false
		}
		e += 8
		o++
	}
	mask := byte(255) << uint(8+e-complexity)
	return h[o]&mask == 0
}

// bytesArg extracts a []byte from a bytes-like JS value: an ArrayBuffer, a
// TypedArray, or a DataView. Per goja's documented behavior this is
// zero-copy - the returned slice is backed by the JS buffer's own storage,
// not a fresh copy - so callers that hand the result to another goroutine
// (hostEmit) must copy it themselves; callers that consume it synchronously
// within the same call (base64.encode, Socket.send) don't need to.
func bytesArg(vm *goja.Runtime, v goja.Value) ([]byte, error) {
	var b []byte
	if err := vm.ExportTo(v, &b); err != nil {
		return nil, fmt.Errorf("expected an ArrayBuffer or TypedArray: %w", err)
	}
	return b, nil
}

// ---- base64.encode / base64.decode ----
//
// The Go<->JS packet boundary itself (write()/emit()) carries raw bytes as
// ArrayBuffers, not base64 - these exist for scripts whose OWN wire format
// needs a text encoding (mailru's cursor field, boards' JSON, ...), so they
// can use the host's native codec instead of a hand-rolled JS one.
func registerBase64(vm *goja.Runtime) {
	b64Obj := vm.NewObject()
	b64Obj.Set("encode", func(call goja.FunctionCall) goja.Value {
		b, err := bytesArg(vm, call.Argument(0))
		if err != nil {
			panic(vm.NewTypeError(err.Error()))
		}
		return vm.ToValue(base64.StdEncoding.EncodeToString(b))
	})
	b64Obj.Set("decode", func(call goja.FunctionCall) goja.Value {
		b, err := base64.StdEncoding.DecodeString(call.Argument(0).String())
		if err != nil {
			panic(vm.NewTypeError(err.Error()))
		}
		return vm.ToValue(vm.NewArrayBuffer(b))
	})
	vm.Set("base64", b64Obj)
}

// dialer returns the shared, netbind-wrapped dialer every script transport's
// HTTP and WebSocket connections dial through, so a script transport binds
// to --inbound=tun exactly like every native transport does.
func (t *ScriptTransport) dialer() *net.Dialer {
	return netbind.Wrap(&net.Dialer{
		Timeout:   15 * time.Second,
		KeepAlive: 30 * time.Second,
	})
}

// ---- http.fetch ----

func registerHTTP(vm *goja.Runtime, t *ScriptTransport) {
	httpObj := vm.NewObject()
	httpObj.Set("fetch", makeFetchFn(vm, t, t.httpClient, t.httpClientNoRedirect))

	// newSession() gives a script its own isolated cookie jar (+ matching
	// follow/manual http.Client pair, same netbind-wrapped dialer and
	// connection pool as the default session). Needed whenever a script
	// juggles several independent logical sessions on the same domain -
	// cupsonline's 4 rooms each get their own csrftoken/session cookies,
	// and sharing the one default jar between them would let the last
	// room's authorize() call silently clobber every earlier room's
	// cookies (cookie names collide across sessions on one domain; jars
	// don't namespace by "logical session", only by domain+path).
	httpObj.Set("newSession", func(call goja.FunctionCall) goja.Value {
		jar, err := cookiejar.New(nil)
		if err != nil {
			panic(vm.NewGoError(err))
		}
		client, clientNoRedirect := t.buildHTTPClients(jar)

		sessObj := vm.NewObject()
		sessObj.Set("fetch", makeFetchFn(vm, t, client, clientNoRedirect))
		// An ad-hoc session has no fixed manifest domain to default to (it
		// might fetch anywhere), so - unlike the default cookieJar, which
		// reads info().cookieDomain - get/set take the URL explicitly, the
		// same URL the script fetched or is about to.
		cookiesObj := vm.NewObject()
		cookiesObj.Set("get", func(call goja.FunctionCall) goja.Value {
			u, err := url.Parse(call.Argument(0).String())
			if err != nil {
				panic(vm.NewTypeError(err.Error()))
			}
			out := vm.NewObject()
			for _, c := range jar.Cookies(u) {
				out.Set(c.Name, c.Value)
			}
			return out
		})
		cookiesObj.Set("set", func(call goja.FunctionCall) goja.Value {
			u, err := url.Parse(call.Argument(0).String())
			if err != nil {
				panic(vm.NewTypeError(err.Error()))
			}
			obj, ok := call.Argument(1).(*goja.Object)
			if !ok {
				return goja.Undefined()
			}
			domain := ""
			if v := call.Argument(2); v != nil && !goja.IsUndefined(v) {
				domain = v.String()
			}
			var cookies []*http.Cookie
			for _, k := range obj.Keys() {
				cookies = append(cookies, &http.Cookie{Name: k, Value: obj.Get(k).String(), Path: "/", Domain: domain})
			}
			jar.SetCookies(u, cookies)
			return goja.Undefined()
		})
		sessObj.Set("cookies", cookiesObj)
		return sessObj
	})

	vm.Set("http", httpObj)
}

type fetchResult struct {
	status   int
	finalURL string
	body     string
	headers  map[string]string
}

// makeFetchFn builds the actual JS-callable fetch(opts) closure over one
// specific client pair - the script's default session (registerHTTP) or an
// ad-hoc one (newSession).
func makeFetchFn(vm *goja.Runtime, t *ScriptTransport, client, clientNoRedirect *http.Client) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		opts, _ := call.Argument(0).(*goja.Object)
		if opts == nil {
			panic(vm.NewTypeError("fetch(opts): opts must be an object"))
		}
		method := "GET"
		if v := opts.Get("method"); v != nil && !goja.IsUndefined(v) {
			method = strings.ToUpper(v.String())
		}
		reqURL := opts.Get("url").String()
		var body string
		if v := opts.Get("body"); v != nil && !goja.IsUndefined(v) {
			body = v.String()
		}
		headers := map[string]string{}
		if h, ok := opts.Get("headers").(*goja.Object); ok {
			for _, k := range h.Keys() {
				headers[k] = h.Get(k).String()
			}
		}
		// redirect: "manual" (default "follow") - a script that needs to
		// inspect or react to a Location header itself (captcha detection,
		// a manual hop-by-hop chain) asks for this instead of getting the
		// final response after Go auto-follows.
		manual := false
		if v := opts.Get("redirect"); v != nil && !goja.IsUndefined(v) {
			manual = v.String() == "manual"
		}

		promise, resolve, reject := vm.NewPromise()
		utils.SafeGo("script."+t.name+".fetch", func() {
			c := client
			if manual {
				c = clientNoRedirect
			}
			out, err := doFetch(c, method, reqURL, body, headers)
			t.loop.RunOnLoop(func(vm *goja.Runtime) {
				if err != nil {
					_ = reject(vm.NewGoError(err))
					return
				}
				resObj := vm.NewObject()
				resObj.Set("status", out.status)
				resObj.Set("url", out.finalURL)
				resObj.Set("body", out.body)
				hdrs := vm.NewObject()
				for k, v := range out.headers {
					hdrs.Set(k, v)
				}
				resObj.Set("headers", hdrs)
				_ = resolve(resObj)
			})
		})
		return vm.ToValue(promise)
	}
}

func doFetch(client *http.Client, method, reqURL, body string, headers map[string]string) (fetchResult, error) {
	req, err := http.NewRequest(method, reqURL, strings.NewReader(body))
	if err != nil {
		return fetchResult{}, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fetchResult{}, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return fetchResult{}, err
	}
	out := fetchResult{
		status:  resp.StatusCode,
		body:    string(b),
		headers: map[string]string{},
	}
	if resp.Request != nil && resp.Request.URL != nil {
		// Final URL after any redirects - this is how a script detects it
		// got bounced to a captcha/login page instead of the API it asked for.
		out.finalURL = resp.Request.URL.String()
	} else {
		out.finalURL = reqURL
	}
	for k := range resp.Header {
		out.headers[k] = resp.Header.Get(k)
	}
	return out, nil
}

// ---- ws.open ----

// wsWriteTimeout bounds one WebSocket write from a script (the native
// transports use 15-20 s).
const wsWriteTimeout = 20 * time.Second

func registerWS(vm *goja.Runtime, t *ScriptTransport) {
	wsObj := vm.NewObject()
	wsObj.Set("open", func(call goja.FunctionCall) goja.Value {
		reqURL := call.Argument(0).String()
		headers := http.Header{}
		if h, ok := call.Argument(1).(*goja.Object); ok {
			for _, k := range h.Keys() {
				headers.Set(k, h.Get(k).String())
			}
		}
		// opts.readTimeoutMs: reset before every read; a connection that
		// falls silent for longer than this (dead NAT binding, half-open
		// TCP) fires onclose instead of blocking the read goroutine
		// forever. 0/omitted = no deadline, matching every native
		// transport except boards (which needs one - see boardsReadDeadline
		// in boards.go).
		var readTimeout time.Duration
		// opts.compress: offer permessage-deflate on the upgrade. Some servers
		// (Mail.ru's nginx) answer 409 Conflict to an upgrade that does not
		// carry the Sec-WebSocket-Extensions header, which gorilla only sends
		// when compression is enabled - without it the handshake is rejected
		// before any auth. Off by default, matching the native transports that
		// do not need it (issue #163, the native mailru sets it too).
		var compress bool
		if opts, ok := call.Argument(2).(*goja.Object); ok {
			if v := opts.Get("readTimeoutMs"); v != nil && !goja.IsUndefined(v) {
				readTimeout = time.Duration(v.ToInteger()) * time.Millisecond
			}
			if v := opts.Get("compress"); v != nil && !goja.IsUndefined(v) {
				compress = v.ToBoolean()
			}
		}

		promise, resolve, reject := vm.NewPromise()
		utils.SafeGo("script."+t.name+".ws-dial", func() {
			dialer := websocket.Dialer{
				HandshakeTimeout:  15 * time.Second,
				NetDialContext:    t.dialer().DialContext,
				EnableCompression: compress,
			}
			conn, resp, err := dialer.Dial(reqURL, headers)
			t.loop.RunOnLoop(func(vm *goja.Runtime) {
				if err != nil {
					status := 0
					if resp != nil {
						status = resp.StatusCode
					}
					_ = reject(vm.NewGoError(fmt.Errorf("ws dial (http %d): %w", status, err)))
					return
				}
				sock := newSocket(vm, t, conn, readTimeout)
				_ = resolve(sock.obj)
			})
		})
		return vm.ToValue(promise)
	})
	vm.Set("ws", wsObj)
}

// socket wraps one live WebSocket connection and dispatches its reads onto
// the transport's own event loop, so JS never has to poll or block on I/O.
// The onmessage/onclose/onerror handlers are looked up lazily on every
// dispatch (call.obj.Get("onmessage")) rather than captured once, so a
// script can (re)assign them any time after open() resolves.
type socket struct {
	obj         *goja.Object
	conn        *websocket.Conn
	readTimeout time.Duration
}

func newSocket(vm *goja.Runtime, t *ScriptTransport, conn *websocket.Conn, readTimeout time.Duration) *socket {
	s := &socket{obj: vm.NewObject(), conn: conn, readTimeout: readTimeout}

	// send(text) writes a text frame (Socket.IO/JSON protocols - mailru,
	// boards); send(bytes) - an ArrayBuffer/TypedArray - writes a binary
	// frame with no text conversion, for a transport with its own binary
	// wire protocol.
	s.obj.Set("send", func(call goja.FunctionCall) goja.Value {
		// send runs on the script's event loop: a write into a half-open
		// connection (a dead NAT binding, a network change) must not block it
		// - and with it every timer, callback and Stop - until the kernel
		// gives up, minutes later. The native transports bound each write the
		// same way.
		_ = conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
		arg := call.Argument(0)
		if _, isObj := arg.(*goja.Object); isObj {
			b, err := bytesArg(vm, arg)
			if err == nil {
				if werr := conn.WriteMessage(websocket.BinaryMessage, b); werr != nil {
					panic(vm.ToValue(werr.Error()))
				}
				return goja.Undefined()
			}
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(arg.String())); err != nil {
			panic(vm.ToValue(err.Error()))
		}
		return goja.Undefined()
	})
	s.obj.Set("close", func(call goja.FunctionCall) goja.Value {
		_ = conn.Close()
		return goja.Undefined()
	})

	t.addCloser(func() { _ = conn.Close() })
	utils.SafeGo("script."+t.name+".ws-read", func() { s.readLoop(t) })
	return s
}

func (s *socket) readLoop(t *ScriptTransport) {
	for {
		if s.readTimeout > 0 {
			_ = s.conn.SetReadDeadline(time.Now().Add(s.readTimeout))
		}
		mt, msg, err := s.conn.ReadMessage()
		if err != nil {
			t.loop.RunOnLoop(func(vm *goja.Runtime) {
				if fn, ok := goja.AssertFunction(s.obj.Get("onclose")); ok {
					_, _ = fn(s.obj, vm.ToValue(err.Error()))
				}
			})
			return
		}
		binary := mt == websocket.BinaryMessage
		data := msg
		t.loop.RunOnLoop(func(vm *goja.Runtime) {
			fn, ok := goja.AssertFunction(s.obj.Get("onmessage"))
			if !ok {
				return
			}
			var arg goja.Value
			if binary {
				arg = vm.ToValue(vm.NewArrayBuffer(data))
			} else {
				arg = vm.ToValue(string(data))
			}
			_, _ = fn(s.obj, arg)
		})
	}
}

// ---- cookieJar.get / cookieJar.set ----

func registerCookieJar(vm *goja.Runtime, t *ScriptTransport) {
	jarObj := vm.NewObject()
	jarObj.Set("get", func(call goja.FunctionCall) goja.Value {
		out := vm.NewObject()
		u := t.cookieBaseURL()
		for _, c := range t.cookieJar.Cookies(u) {
			out.Set(c.Name, c.Value)
		}
		return out
	})
	// set(values, domain?) - domain defaults to info().cookieDomain's host.
	// An explicit domain is for a transport whose cookies must apply across
	// subdomains (e.g. Yandex: a cookie obtained via disk.yandex.ru needs to
	// reach docs.yandex.ru too), matching the Domain attribute a real
	// browser would set from a Set-Cookie response header.
	jarObj.Set("set", func(call goja.FunctionCall) goja.Value {
		obj, ok := call.Argument(0).(*goja.Object)
		if !ok {
			return goja.Undefined()
		}
		domain := ""
		if v := call.Argument(1); v != nil && !goja.IsUndefined(v) {
			domain = v.String()
		}
		var cookies []*http.Cookie
		for _, k := range obj.Keys() {
			cookies = append(cookies, &http.Cookie{Name: k, Value: obj.Get(k).String(), Path: "/", Domain: domain})
		}
		t.cookieJar.SetCookies(t.cookieBaseURL(), cookies)
		return goja.Undefined()
	})
	vm.Set("cookieJar", jarObj)
}

func (t *ScriptTransport) cookieBaseURL() *url.URL {
	domain := t.info.CookieDomain
	if domain == "" {
		domain = "https://localhost/"
	}
	u, err := url.Parse(domain)
	if err != nil {
		u, _ = url.Parse("https://localhost/")
	}
	return u
}

// ---- emit / setState / raise (JS -> Go) ----

// hostEmit delivers one received application packet up to the core.
// `pkt` is an ArrayBuffer/TypedArray - emit(base64.decode(str)) for a
// transport whose own wire format is textual, emit(bytes) directly for one
// that already has bytes. bytesArg's export is zero-copy (backed by the
// JS buffer's own storage), so this copies once before handing off: the
// downstream call runs on a fresh goroutine, and whatever the
// manager/session does with it (decrypt, reassemble, route) must never run
// on this transport's own event-loop goroutine, or a slow downstream
// stalls this transport's reconnects and keepalives too.
func hostEmit(vm *goja.Runtime, t *ScriptTransport) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		raw, err := bytesArg(vm, call.Argument(0))
		if err != nil {
			utils.Debugf("[SCRIPT:%s] emit: %v", t.name, err)
			return goja.Undefined()
		}
		decoded := append([]byte(nil), raw...)
		t.RecordReceive(len(decoded))
		utils.SafeGo("script."+t.name+".recv", func() { t.CallReceive(decoded) })
		return goja.Undefined()
	}
}

// hostSetState carries the script's own view of link health. It is the only
// way IsConnected() ever flips true - a script that never calls
// setState("connected") never looks connected to the core, no matter what
// it's doing on the wire.
func hostSetState(t *ScriptTransport) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		state := call.Argument(0).String()
		errMsg := ""
		if v := call.Argument(1); v != nil && !goja.IsUndefined(v) {
			errMsg = v.String()
		}
		t.SetConnected(state == "connected")
		if state == "reconnecting" {
			t.RecordReconnect()
		}
		t.setLastState(state, errMsg)
		utils.Debugf("[SCRIPT:%s] state=%s err=%q", t.name, state, errMsg)
		return goja.Undefined()
	}
}

// captchaRaiseKinds are the raise() kinds that also reach SetErrorNotifier,
// in addition to the generic EventHandler every kind reaches: a reactive
// mid-session check (captchaRequired, matching the native yandex/mailru
// transports' own wording) and a proactive "nothing is configured yet"
// signal (needsSetup) a script can raise right from open() before it even
// tries to connect - manager.go treats both identically, so the choice is
// purely which reads better in the script's own code.
var captchaRaiseKinds = map[string]bool{"captchaRequired": true, "needsSetup": true}

// hostRaise carries out-of-band events upward. Every kind reaches whatever
// registered via SetEventHandler (mirrors the existing CookieExchanger
// pattern used by the native yandex/mailru transports, generalized to any
// kind); captchaRaiseKinds additionally reach SetErrorNotifier - the same
// path those native transports' own captcha/login signal takes - carrying
// payload.url (a real site, or the script's own httpserver.listen() address)
// or payload.html (the script's own page, see js/template_html.html) and
// payload.reason. For those kinds the payload is checked first (see
// setuppage.go) and a bad one throws a TypeError into the script.
func hostRaise(vm *goja.Runtime, t *ScriptTransport) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		kind := call.Argument(0).String()
		payload := map[string]interface{}{}
		if obj, ok := call.Argument(1).(*goja.Object); ok {
			for _, k := range obj.Keys() {
				payload[k] = obj.Get(k).Export()
			}
		}
		str := func(key string) string { s, _ := payload[key].(string); return s }
		reason := str("reason")
		if captchaRaiseKinds[kind] {
			checked, err := t.checkSetupPayload(str("html"), str("url"), reason)
			if err != nil {
				panic(vm.NewTypeError("raise(" + kind + "): " + err.Error()))
			}
			reason = checked
			payload["reason"] = reason
		}
		if kind == "roomList" {
			if rooms, _ := payload["rooms"].(string); rooms != "" {
				t.setRoomList(rooms)
			}
		}
		if h := t.eventHandler(); h != nil {
			h(kind, payload)
		}
		if captchaRaiseKinds[kind] {
			if en := t.errorNotifier(); en != nil {
				en(fmt.Errorf("script: %s", kind), t.name, str("url"), str("html"), reason)
			}
		}
		return goja.Undefined()
	}
}

// ---- concurrency.pool ----
//
// Volga-style transports need real fan-out (the native relayClient runs
// N=2000 workers draining a queue into batched HTTP POSTs) - goja is
// single-threaded, so that concurrency has to come from Go, not from a
// script pretending to run N workers itself. concurrency.pool(n) is the
// abstract, reusable primitive for that: run(fn) gates at most n
// concurrent in-flight fn() calls through a Go semaphore, but fn's own
// work (typically an http.fetch call) still executes on its own goroutine
// with true OS-level concurrency exactly like any other fetch - this
// primitive only adds the bounded-fan-out bookkeeping so a script doesn't
// have to hand-roll a counting semaphore to get it.
func registerConcurrency(vm *goja.Runtime, t *ScriptTransport) {
	concObj := vm.NewObject()
	concObj.Set("pool", func(call goja.FunctionCall) goja.Value {
		n := int(call.Argument(0).ToInteger())
		if n < 1 {
			n = 1
		}
		sem := make(chan struct{}, n)

		poolObj := vm.NewObject()
		poolObj.Set("run", func(call goja.FunctionCall) goja.Value {
			fn, isFn := goja.AssertFunction(call.Argument(0))
			if !isFn {
				panic(vm.NewTypeError("pool.run(fn): fn must be a function"))
			}
			promise, resolve, reject := vm.NewPromise()
			utils.SafeGo("script."+t.name+".pool-acquire", func() {
				sem <- struct{}{}
				released := false
				release := func() {
					if !released {
						released = true
						<-sem
					}
				}
				t.loop.RunOnLoop(func(vm *goja.Runtime) {
					result, err := fn(goja.Undefined())
					if err != nil {
						release()
						_ = reject(vm.NewGoError(err))
						return
					}
					settleThenable(vm, result, resolve, reject, release)
				})
			})
			return vm.ToValue(promise)
		})
		poolObj.Set("size", func(call goja.FunctionCall) goja.Value {
			return vm.ToValue(len(sem))
		})
		return poolObj
	})
	vm.Set("concurrency", concObj)
}

// settleThenable resolves/rejects (resolve/reject) with result, first
// awaiting result's own .then() if it looks like a thenable (a Promise fn
// returned) - the same duck-typing native await uses. release runs exactly
// once, whichever way it settles.
func settleThenable(vm *goja.Runtime, result goja.Value, resolve, reject func(interface{}) error, release func()) {
	if obj, ok := result.(*goja.Object); ok {
		if thenFn, ok := goja.AssertFunction(obj.Get("then")); ok {
			onFulfilled := func(call goja.FunctionCall) goja.Value {
				release()
				_ = resolve(call.Argument(0))
				return goja.Undefined()
			}
			onRejected := func(call goja.FunctionCall) goja.Value {
				release()
				_ = reject(call.Argument(0))
				return goja.Undefined()
			}
			if _, err := thenFn(obj, vm.ToValue(onFulfilled), vm.ToValue(onRejected)); err == nil {
				return
			}
		}
	}
	release()
	_ = resolve(result)
}

// ---- udp.open ----
//
// Preemptive datagram support: a dialed (connected, fixed-remote) UDP
// socket, same shape as ws.open's Socket so scripts already know the
// pattern. Listen-from-any-peer sockets aren't implemented yet (nothing
// in this codebase needs them - every transport, native or scripted,
// dials a known exit/relay address) - only the dial side exists so far,
// and it goes through the same netbind-wrapped dialer as everything else.
func registerUDP(vm *goja.Runtime, t *ScriptTransport) {
	udpObj := vm.NewObject()
	udpObj.Set("open", func(call goja.FunctionCall) goja.Value {
		remoteAddr := call.Argument(0).String()
		var readTimeout time.Duration
		if opts, ok := call.Argument(1).(*goja.Object); ok {
			if v := opts.Get("readTimeoutMs"); v != nil && !goja.IsUndefined(v) {
				readTimeout = time.Duration(v.ToInteger()) * time.Millisecond
			}
		}

		promise, resolve, reject := vm.NewPromise()
		utils.SafeGo("script."+t.name+".udp-dial", func() {
			conn, err := t.dialer().DialContext(context.Background(), "udp", remoteAddr)
			t.loop.RunOnLoop(func(vm *goja.Runtime) {
				if err != nil {
					_ = reject(vm.NewGoError(err))
					return
				}
				sock := newDatagramSocket(vm, t, conn, readTimeout)
				_ = resolve(sock.obj)
			})
		})
		return vm.ToValue(promise)
	})
	vm.Set("udp", udpObj)
}

type datagramSocket struct {
	obj         *goja.Object
	conn        net.Conn
	readTimeout time.Duration
}

func newDatagramSocket(vm *goja.Runtime, t *ScriptTransport, conn net.Conn, readTimeout time.Duration) *datagramSocket {
	s := &datagramSocket{obj: vm.NewObject(), conn: conn, readTimeout: readTimeout}

	s.obj.Set("send", func(call goja.FunctionCall) goja.Value {
		b, err := bytesArg(vm, call.Argument(0))
		if err != nil {
			panic(vm.NewTypeError(err.Error()))
		}
		if _, werr := conn.Write(b); werr != nil {
			panic(vm.ToValue(werr.Error()))
		}
		return goja.Undefined()
	})
	s.obj.Set("close", func(call goja.FunctionCall) goja.Value {
		_ = conn.Close()
		return goja.Undefined()
	})

	t.addCloser(func() { _ = conn.Close() })
	utils.SafeGo("script."+t.name+".udp-read", func() { s.readLoop(t) })
	return s
}

func (s *datagramSocket) readLoop(t *ScriptTransport) {
	buf := make([]byte, 65536)
	for {
		if s.readTimeout > 0 {
			_ = s.conn.SetReadDeadline(time.Now().Add(s.readTimeout))
		}
		n, err := s.conn.Read(buf)
		if err != nil {
			t.loop.RunOnLoop(func(vm *goja.Runtime) {
				if fn, ok := goja.AssertFunction(s.obj.Get("onclose")); ok {
					_, _ = fn(s.obj, vm.ToValue(err.Error()))
				}
			})
			return
		}
		data := append([]byte(nil), buf[:n]...) // buf is reused next iteration
		t.loop.RunOnLoop(func(vm *goja.Runtime) {
			if fn, ok := goja.AssertFunction(s.obj.Get("onmessage")); ok {
				_, _ = fn(s.obj, vm.ToValue(vm.NewArrayBuffer(data)))
			}
		})
	}
}
