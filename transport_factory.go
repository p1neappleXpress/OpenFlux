package main

import (
	"fmt"
	"strconv"

	"github.com/p1neappleXpress/OpenFlux/transport"
	"github.com/p1neappleXpress/OpenFlux/transport/control"
	"github.com/p1neappleXpress/OpenFlux/transport/cupsonline"
	"github.com/p1neappleXpress/OpenFlux/transport/mailru"
	"github.com/p1neappleXpress/OpenFlux/transport/manager"
	"github.com/p1neappleXpress/OpenFlux/transport/oneme"
	"github.com/p1neappleXpress/OpenFlux/transport/yandex"
)

// yandexCookiesFile is --yandex-cookies-file: a Netscape cookies.txt with
// a Yandex login that every vyandex transport starts with.
var yandexCookiesFile string

func newVolgaTransport(docURL string, cfg transport.TransportConfig) (transport.Transport, error) {
	t := yandex.NewYandexVolgaTransport(docURL, cfg)
	if yandexCookiesFile != "" {
		if err := t.LoadCookieFile(yandexCookiesFile); err != nil {
			return nil, err
		}
	}
	return t, nil
}

// transportFactory builds a raw transport from a control.TransportConfig.
// It is the single place that knows every transport package. main.go passes
// it into manager.New, and manager calls it whenever the peer asks the exit
// to bring up an additional transport at runtime.
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
		switch cfg.Type {
		case "yandex":
			return yandex.NewYandexDocsTransport(cfg.URL, baseCfg), nil
		case "vyandex":
			return newVolgaTransport(cfg.URL, baseCfg)
		case "boards":
			return yandex.NewBoardsTransport(cfg.URL, baseCfg), nil
		case "mailru":
			return mailru.NewMailruDocsTransport(cfg.URL, baseCfg), nil
		case "cupsonline":
			return cupsonline.NewCupsonlineTransport(cfg.URL, baseCfg, !isExit), nil
		case "oneme":
			token, _ := cfg.Params["token"].(string)
			uidStr, _ := cfg.Params["uid"].(string)
			uid, _ := strconv.ParseInt(uidStr, 10, 64)
			exit, _ := cfg.Params["exit"].(bool)
			return oneme.NewOneMeTransport(exit, token, uid, baseCfg), nil
		case "direct":
			dcfg := transport.DefaultDirectConfig()
			if v, ok := cfg.Params["listen"].(string); ok {
				dcfg.ListenAddr = v
			}
			if v, ok := cfg.Params["dial"].(string); ok {
				dcfg.DialAddr = v
			}
			if v, ok := cfg.Params["is_exit"].(bool); ok {
				dcfg.IsExit = v
			}
			return transport.NewDirectTransport(baseCfg, dcfg), nil
		default:
			return nil, fmt.Errorf("factory: unknown transport type %q", cfg.Type)
		}
	}
}
