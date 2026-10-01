package mobile

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/p1neappleXpress/OpenFlux/socks5"
	"github.com/p1neappleXpress/OpenFlux/streamproxy"
	"github.com/p1neappleXpress/OpenFlux/transport"
	"github.com/p1neappleXpress/OpenFlux/tunnel"
	"github.com/p1neappleXpress/OpenFlux/utils"
)

// StartStreamProxy is StartProxy for the phpbox stream mode: the exit is a PHP
// node on an ordinary web host (deploy/phpbox, put there by PhpCall "deploy"),
// reached over a cups.online room ("cupsonline", url = the room's address) or a
// Mail.ru document ("mailru", url = its public link). The local SOCKS5 proxy
// hands every connection to a stream of the mux. TCP only (ports 80 and 443 at
// the exit); no key, no session: the apps' own TLS protects the content.
//
// Everything else about it is StartProxy's: "" on success or a readable error,
// ProxyIsConnected / ProxyBytesSent / ProxyBytesReceived / StopProxy work on
// it, username/password guard the listener, bypassDomains go direct.
func StartStreamProxy(transportType, url, listenAddr, username, password, bypassDomains string) string {
	proxy.mu.Lock()
	if proxy.running {
		proxy.mu.Unlock()
		return ""
	}
	proxy.mu.Unlock()

	switch transportType {
	case "cupsonline", "mailru":
	default:
		return fmt.Sprintf("Режим без сервера не работает через транспорт %q: нужен cups.online или Mail.ru", transportType)
	}
	if strings.TrimSpace(url) == "" {
		return "Адрес ноды не указан"
	}

	utils.SetLevel(int(debugLevel.Load()))
	utils.SetLogSink(appendLog)
	appendLog("[ANDROID] Запуск прокси в режиме без сервера (PHP-хостинг)")

	carrier, err := newRawTransport(transportType, strings.TrimSpace(url), nil, transport.DefaultConfig(), false)
	if err != nil {
		appendLog(fmt.Sprintf("[ERROR] Ошибка запуска прокси: %v", err))
		return err.Error()
	}
	var wrap func(socks5.Dialer) socks5.Dialer
	if strings.TrimSpace(bypassDomains) != "" {
		list := strings.Split(bypassDomains, "\n")
		wrap = func(d socks5.Dialer) socks5.Dialer { return newSplitDialer(d, list) }
	}
	p, err := streamproxy.Start(streamproxy.Options{
		Carrier: carrier, Socks: listenAddr, Username: username, Password: password, Wrap: wrap, Label: transportType,
	})
	if err != nil {
		appendLog(fmt.Sprintf("[ERROR] Не удалось запустить прокси: %v", err))
		if strings.Contains(err.Error(), "bind") {
			return fmt.Sprintf("Порт %s уже занят", listenAddr)
		}
		return err.Error()
	}

	proxy.mu.Lock()
	proxy.running = true
	proxy.stream = p
	proxy.mu.Unlock()
	appendLog(fmt.Sprintf("[SUCCESS] SOCKS5-прокси слушает %s (режим без сервера)", listenAddr))
	return ""
}

// StartStreamPacket is Start for the stream mode: the device's IP packets (the
// Android VpnService's tun) go through Send, and Read returns the packets for
// it, exactly as in the classic packet mode, so the VPN plumbing is unchanged.
// Inside, a local stack terminates each TCP connection and opens a stream to
// the same destination at the PHP exit; DNS is answered locally with fake
// addresses and the name is opened at the exit. TCP on ports 80 and 443 only:
// QUIC, other UDP and IPv6 are dropped, and browsers fall back to TCP. Returns
// "" on success or a readable error, like Start.
func StartStreamPacket(transportType, url string) string {
	switch transportType {
	case "cupsonline", "mailru":
	default:
		return fmt.Sprintf("Режим без сервера не работает через транспорт %q: нужен cups.online или Mail.ru", transportType)
	}
	if strings.TrimSpace(url) == "" {
		return "Адрес ноды не указан"
	}
	return startPacket(func() (transport.Transport, error) {
		appendLog("[ANDROID] Запуск туннеля в режиме без сервера (PHP-хостинг)")
		carrier, err := newRawTransport(transportType, strings.TrimSpace(url), nil, transport.DefaultConfig(), false)
		if err != nil {
			return nil, err
		}
		return tunnel.NewStreamNet(carrier), nil
	})
}

// StreamExitIP asks the exit its own public IP through the running stream tunnel
// (the packet/VPN mode), so the app can show it where it otherwise could not:
// in VPN mode the app is outside its own tunnel and cannot reach the internet to
// ask. It opens one stream beside the device's packets, fetches api.ipify.org and
// returns the IP; "" when no stream tunnel is running, or "error: ..." on failure.
func StreamExitIP() string {
	client.mu.Lock()
	t := client.transport
	client.mu.Unlock()
	sn, ok := t.(interface {
		DialStream(context.Context, string, int) (net.Conn, error)
	})
	if !ok {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	hc := &http.Client{
		Timeout: 25 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
				host, portStr, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				port, _ := strconv.Atoi(portStr)
				return sn.DialStream(ctx, host, port)
			},
			DisableKeepAlives: true,
		},
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.ipify.org", nil)
	resp, err := hc.Do(req)
	if err != nil {
		return "error: " + err.Error()
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return "error: " + err.Error()
	}
	return strings.TrimSpace(string(body))
}
