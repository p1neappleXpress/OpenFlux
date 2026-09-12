package main

import (
	"context"
	"crypto/tls"
	"net"
	"time"

	"universal-bypass-tool/utils"
)

// dotServer — DNS-over-TLS endpoint (addr:853 + TLS SNI).
type dotServer struct {
	addr string
	sni  string
}

// dotServers — порядок обхода. Первый, который ответит, выигрывает.
// Yandex идёт первым: он достижим в РФ и не блокируется операторами.
var dotServers = []dotServer{
	{"77.88.8.8:853", "common.dot.dns.yandex.net"},
	{"8.8.8.8:853", "dns.google"},
	{"1.1.1.1:853", "cloudflare-dns.com"},
}

// dialSecureDNS открывает DoT-соединение для net.Resolver, перебирая
// серверы по очереди.
func dialSecureDNS(ctx context.Context, _, _ string) (net.Conn, error) {
	var lastErr error
	for _, s := range dotServers {
		d := tls.Dialer{
			NetDialer: &net.Dialer{Timeout: 6 * time.Second},
			Config:    &tls.Config{ServerName: s.sni, MinVersion: tls.VersionTLS12},
		}
		conn, err := d.DialContext(ctx, "tcp", s.addr)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		utils.Debugf("[DNS] DoT %s failed: %v", s.addr, err)
	}
	return nil, lastErr
}

// installDoTResolver подменяет net.DefaultResolver на DoT.
// Идемпотентно: повторный вызов ничего не ломает.
func installDoTResolver() {
	net.DefaultResolver = &net.Resolver{
		PreferGo:     true,
		StrictErrors: false,
		Dial:         dialSecureDNS,
	}
	utils.Debugf("[DNS] DoT resolver installed")
}
