package mobile

import (
	"fmt"
	"strings"

	"github.com/p1neappleXpress/OpenFlux/socks5"
	"github.com/p1neappleXpress/OpenFlux/streamproxy"
	"github.com/p1neappleXpress/OpenFlux/transport"
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
