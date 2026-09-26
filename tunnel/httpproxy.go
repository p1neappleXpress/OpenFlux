package tunnel

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"

	"openflux/transport"
	"openflux/utils"
)

// NewSideTunnel builds a client TCP stack on a PortDemux side, confined to
// local ports [lo, hi] so its replies can be told apart from the main path.
func NewSideTunnel(trans transport.Transport, lo, hi uint16) (*TCPTunnel, error) {
	t := NewTCPTunnel(trans, false)
	if err := t.gvisorStack.SetPortRange(lo, hi); err != nil {
		t.Close()
		return nil, fmt.Errorf("side tunnel port range %d-%d: %s", lo, hi, err)
	}
	return t, nil
}

// ServeHTTPProxy serves a plain HTTP proxy (CONNECT and absolute-URI
// requests) on ln, opening every upstream connection with dial. It is what
// a browser (e.g. an app's WebView) is pointed at to reach the Internet from
// the exit's address: HTTP proxies are supported by every embedded browser,
// SOCKS5 is not.
func ServeHTTPProxy(ln net.Listener, dial func(address string) (net.Conn, error)) error {
	upstream := &http.Transport{
		DialContext: func(_ context.Context, _, address string) (net.Conn, error) {
			return dial(address)
		},
	}
	return http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			proxyConnect(w, r, dial)
			return
		}
		proxyForward(w, r, upstream)
	}))
}

func proxyConnect(w http.ResponseWriter, r *http.Request, dial func(string) (net.Conn, error)) {
	up, err := dial(r.Host)
	if err != nil {
		utils.Debugf("[HTTPPROXY] CONNECT %s: %v", r.Host, err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		up.Close()
		http.Error(w, "hijacking unsupported", http.StatusInternalServerError)
		return
	}
	client, buffered, err := hj.Hijack()
	if err != nil {
		up.Close()
		return
	}
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		client.Close()
		up.Close()
		return
	}
	var once sync.Once
	closeBoth := func() { client.Close(); up.Close() }
	go func() {
		defer once.Do(closeBoth)
		// Bytes the client sent right after the CONNECT line may already
		// sit in the server's read buffer.
		_, _ = io.Copy(up, buffered)
	}()
	go func() {
		defer once.Do(closeBoth)
		_, _ = io.Copy(client, up)
	}()
}

func proxyForward(w http.ResponseWriter, r *http.Request, upstream http.RoundTripper) {
	if !r.URL.IsAbs() {
		http.Error(w, "this is a proxy: absolute URL required", http.StatusBadRequest)
		return
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.Header.Del("Proxy-Connection")
	out.Header.Del("Proxy-Authorization")
	resp, err := upstream.RoundTrip(out)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
