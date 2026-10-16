package main

import (
	"fmt"

	"github.com/p1neappleXpress/OpenFlux/transport"
	"github.com/p1neappleXpress/OpenFlux/transport/control"
	"github.com/p1neappleXpress/OpenFlux/transport/manager"
	"github.com/p1neappleXpress/OpenFlux/transport/registry"
)

// yandexCookiesFile is --yandex-cookies-file: a Netscape cookies.txt with
// a Yandex login that every vyandex transport starts with.
var yandexCookiesFile string

// avitoCookiesStr is --avito-cookies: a raw cookie string or path to a
// Netscape cookies.txt for code.avito.ru.
var avitoCookiesStr string

// transportFactory builds a raw transport from a control.TransportConfig.
// main.go passes it into manager.New, and manager calls it whenever the peer
// asks the exit to bring up an additional transport at runtime. The types are
// the registry's: it is the single place that knows every transport package.
//
// isExit is this process's role: a cupsonline client must never create
// rooms of its own. (It used to be built as an exit everywhere, so a
// Session client with no or dead rooms created four new ones and waited in
// them, where the exit never came.)
func transportFactory(baseCfg transport.TransportConfig, isExit bool) manager.Factory {
	return func(cfg *control.TransportConfig) (transport.Transport, error) {
		if cfg == nil {
			return nil, fmt.Errorf("factory: nil config")
		}
		baseCfg := baseCfg
		if n, _ := cfg.Params["network"].(string); n != "" {
			baseCfg.Network = n // this carrier's network
		}
		return registry.New(cfg.Type, cfg.URL, cfg.Params, registry.Options{
			Base:              baseCfg,
			IsExit:            isExit,
			YandexCookiesFile: yandexCookiesFile,
			AvitoCookies:      avitoCookiesStr,
		})
	}
}
