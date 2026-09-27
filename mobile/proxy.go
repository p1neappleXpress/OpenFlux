// Proxy mode exposes a local SOCKS5 listener backed by the same encrypted
// document transport as the tunnel packet mode, but routed through an
// in-process gVisor TCP/IP stack (tunnel.TCPTunnel) instead of an Android
// VpnService TUN.
// This mirrors exactly what the desktop CLI's client mode already does in
// main.go, so it needs no changes on the exit node / VDS side.
package mobile

import (
	"fmt"
	"strings"
	"sync"

	"openflux/socks5"
	"openflux/transport"
	"openflux/tunnel"
	"openflux/utils"
)

var proxy = proxyState{}

type proxyState struct {
	mu        sync.Mutex
	running   bool
	transport transport.Transport
	tun       *tunnel.TCPTunnel
	server    *socks5.SOCKS5Server
}

// StartProxy launches the local SOCKS5 proxy in classic single-transport
// mode. Returns "" once the listener is bound and the transport handshake
// has started, or a user-readable error. Call ProxyIsConnected to learn when
// the tunnel itself is actually up. Hostname lookups are resolved locally by
// TCPTunnel (the same as the desktop CLI client), so no exit-node changes are
// required. When username is non-empty, the SOCKS5 server requires that
// username/password (e.g. for a proxy bound to 0.0.0.0 and reachable from
// the local network); an empty username leaves it open, as appropriate for a
// loopback-only bind. bypassDomains is a newline-separated list (from the
// Android "Маршрутизация" settings tab) of domains to dial directly instead
// of through the tunnel; pass "" for none.
func StartProxy(transportType, documentURL, encryptionSecret, codec, maxToken, maxUid, listenAddr, username, password, bypassDomains string) string {
	if msg := validateClassic(transportType, documentURL, encryptionSecret); msg != "" {
		return msg
	}
	return startProxyWith(func() (transport.Transport, error) {
		return classicTransport(transportType, documentURL, encryptionSecret, codec, maxToken, maxUid, false)
	}, listenAddr, username, password, bypassDomains)
}

// StartSessionProxy is StartProxy in Session mode, see StartSession.
func StartSessionProxy(specsJSON, encryptionSecret, listenAddr, username, password, bypassDomains string) string {
	return startProxyWith(func() (transport.Transport, error) {
		return buildSession(specsJSON, encryptionSecret, false)
	}, listenAddr, username, password, bypassDomains)
}

func startProxyWith(build func() (transport.Transport, error), listenAddr, username, password, bypassDomains string) string {
	proxy.mu.Lock()
	if proxy.running {
		proxy.mu.Unlock()
		return ""
	}
	proxy.mu.Unlock()

	utils.SetLevel(int(debugLevel.Load()))
	utils.SetLogSink(appendLog)
	appendLog("[ANDROID] Запуск прокси-транспорта")

	trans, err := build()
	if err != nil {
		appendLog(fmt.Sprintf("[ERROR] Ошибка запуска прокси: %v", err))
		detachCaptcha()
		setAuthProxy(nil)
		return err.Error()
	}
	if err := trans.Start(); err != nil {
		appendLog(fmt.Sprintf("[ERROR] Ошибка запуска прокси: %v", err))
		detachCaptcha()
		setAuthProxy(nil)
		return err.Error()
	}

	tun := tunnel.NewTCPTunnel(trans, false)
	var dialer socks5.Dialer = tun
	if strings.TrimSpace(bypassDomains) != "" {
		dialer = newSplitDialer(tun, strings.Split(bypassDomains, "\n"))
	}
	server := socks5.NewSOCKS5Server(listenAddr, dialer)
	if username != "" {
		server.SetAuth(username, password)
	}
	if err := server.Bind(); err != nil {
		_ = trans.Stop()
		appendLog(fmt.Sprintf("[ERROR] Не удалось занять %s: %v", listenAddr, err))
		detachCaptcha()
		setAuthProxy(nil)
		return fmt.Sprintf("Порт %s уже занят", listenAddr)
	}

	proxy.mu.Lock()
	proxy.running = true
	proxy.transport = trans
	proxy.tun = tun
	proxy.server = server
	proxy.mu.Unlock()

	utils.SafeGo("mobile.proxyServe", func() {
		err := server.Start()
		proxy.mu.Lock()
		stillRunning := proxy.running
		proxy.mu.Unlock()
		if stillRunning && err != nil {
			appendLog(fmt.Sprintf("[ERROR] Прокси остановлен: %v", err))
		}
	})

	appendLog(fmt.Sprintf("[SUCCESS] SOCKS5-прокси слушает %s", listenAddr))
	return ""
}

func StopProxy() {
	proxy.mu.Lock()
	server := proxy.server
	trans := proxy.transport
	proxy.running = false
	proxy.transport = nil
	proxy.tun = nil
	proxy.server = nil
	proxy.mu.Unlock()
	detachCaptcha()
	CancelCaptcha()
	setAuthProxy(nil)
	clearRoute()
	appendLog("[ANDROID] Остановка прокси-транспорта")
	if server != nil {
		_ = server.Close()
	}
	if trans != nil {
		_ = trans.Stop()
	}
}

func ProxyIsRunning() bool {
	proxy.mu.Lock()
	defer proxy.mu.Unlock()
	return proxy.running
}

func ProxyIsConnected() bool {
	proxy.mu.Lock()
	trans := proxy.transport
	proxy.mu.Unlock()
	return trans != nil && trans.IsConnected()
}

// ProxyBytesSent and ProxyBytesReceived return running totals relayed
// through the local SOCKS5 server (client -> internet and internet ->
// client respectively) across every connection since StartProxy, for a live
// speed indicator. Both are 0 if the proxy isn't running.
func ProxyBytesSent() int64 {
	proxy.mu.Lock()
	server := proxy.server
	proxy.mu.Unlock()
	if server == nil {
		return 0
	}
	return server.BytesSent()
}

func ProxyBytesReceived() int64 {
	proxy.mu.Lock()
	server := proxy.server
	proxy.mu.Unlock()
	if server == nil {
		return 0
	}
	return server.BytesReceived()
}
