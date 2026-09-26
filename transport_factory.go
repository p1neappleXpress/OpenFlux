package main

import (
	"fmt"
	"strconv"

	"openflux/transport"
	"openflux/transport/control"
	"openflux/transport/cupsonline"
	"openflux/transport/mailru"
	"openflux/transport/manager"
	"openflux/transport/oneme"
	"openflux/transport/yandex"
)

// transportFactory builds a raw transport from a control.TransportConfig.
// It is the single place that knows every transport package. main.go passes
// it into manager.New, and manager calls it whenever the peer asks the exit
// to bring up an additional transport at runtime.
func transportFactory(baseCfg transport.TransportConfig) manager.Factory {
	return func(cfg *control.TransportConfig) (transport.Transport, error) {
		if cfg == nil {
			return nil, fmt.Errorf("factory: nil config")
		}
		switch cfg.Type {
		case "yandex":
			return yandex.NewYandexDocsTransport(cfg.URL, baseCfg), nil
		case "vyandex":
			return yandex.NewYandexVolgaTransport(cfg.URL, baseCfg), nil
		case "boards":
			return yandex.NewBoardsTransport(cfg.URL, baseCfg), nil
		case "mailru":
			return mailru.NewMailruDocsTransport(cfg.URL, baseCfg), nil
		case "cupsonline":
			return cupsonline.NewCupsonlineTransport(cfg.URL, baseCfg, false), nil
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
