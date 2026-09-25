package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	godebug "runtime/debug"
	"strconv"
	"strings"

	_ "github.com/wlynxg/anet"
	"universal-bypass-tool/socks5"
	"universal-bypass-tool/transport"
	"universal-bypass-tool/transport/mailru"
	"universal-bypass-tool/transport/oneme"
	"universal-bypass-tool/transport/yandex"
	"universal-bypass-tool/tunnel"
	"universal-bypass-tool/utils"
)

var (
	globalDocUrl string
	maxToken     string
	maxUid       string
)

// buildMuxTransport turns a document-URL spec into a transport. The spec is a
// comma-separated list: a single URL yields one channel, while multiple URLs
// yield a MultiplexTransport that stripes flows across the documents. Each inner
// document is wrapped in its own AdaptiveTransport, a self-negotiating codec
// that starts in the legacy per-packet format and upgrades to batching once the
// peer proves it speaks batch — so a new build interoperates with an old peer
// (staying legacy) instead of breaking, and runs fast when both are new.
func buildMuxTransport(urlSpec string, factory func(string) transport.Transport) transport.Transport {
	var urls []string
	for _, u := range strings.Split(urlSpec, ",") {
		if u = strings.TrimSpace(u); u != "" {
			urls = append(urls, u)
		}
	}

	if len(urls) <= 1 {
		u := urlSpec
		if len(urls) == 1 {
			u = urls[0]
		}
		return transport.NewAdaptiveTransport(factory(u))
	}

	channels := make([]transport.Transport, 0, len(urls))
	for _, u := range urls {
		channels = append(channels, transport.NewAdaptiveTransport(factory(u)))
	}
	log.Printf("Multiplex: %d channels", len(channels))
	return transport.NewMultiplexTransport(channels)
}

func main() {
	//os.Setenv("GODEBUG", "netdns=go")
	fmt.Print("written by p1neappleXpress\n")

	exitNode := flag.Bool("exit-node", false, "Run as exit node (needs root)")
	client := flag.Bool("client", false, "Run as client")
	debug := flag.Bool("debug", false, "Enable verbose debug logging")
	socksAddr := flag.String("socks5", ":1080", "SOCKS5 address")
	transportType := flag.String("transport", "yandex", "Transport type (yandex, google, custom)")
	flag.StringVar(&globalDocUrl, "url", "http://#", "Document URL for Yandex.Docs transport. Comma-separated list = multiplex across N documents (client and exit node must pass the same list)")
	flag.StringVar(&maxToken, "maxToken", "", "MAX call user id. If u use MAX transport")
	flag.StringVar(&maxUid, "maxUid", "", "MAX Web token. If u use MAX transport")
	localIP := flag.String("local-ip", "", "Exit node egress IP (use a dedicated alias IP so the RST-drop rule can be scoped with -s)")
	directAddr := flag.String("direct-addr", "",
		"host:port for --transport=direct. Exit node listens on it, client dials it. "+
			"Requires --encryption-key-file: a plain TCP carrier exposes the node, so it "+
			"must never run unencrypted.")
	cookiesFile := flag.String("cookies-file", "",
		"Path to a file with cookies (\"a=1; b=2\") for the yandex transport. Watched "+
			"live: solve an interactive captcha in a browser, drop the cookies here, and "+
			"the node picks them up without a restart.")
	encryptionKeyFile := flag.String("encryption-key-file", "",
		"Path to a file holding the shared secret for end-to-end AES-256-GCM. "+
			"Both peers must pass the same secret; empty = no encryption.")
	flag.Parse()

	if *localIP != "" {
		tunnel.SetLocalIP(*localIP)
	}

	// The exit node often runs on a tiny VPS; keep the heap tight under load
	// (GC aggressively). Set GOMEMLIMIT in the environment for a hard soft-cap.
	if *exitNode {
		godebug.SetGCPercent(20)
	}

	if !*exitNode && !*client {
		flag.Usage()
		os.Exit(1)
	}

	if *debug {
		utils.EnableDebug()
	}

	log.Printf("=== Universal Bypass Tool ===")
	log.Printf("Mode: %s", map[bool]string{true: "EXIT NODE", false: "CLIENT"}[*exitNode])
	log.Printf("Transport: %s", *transportType)

	config := transport.DefaultConfig()
	var trans transport.Transport

	switch *transportType {
	case "boards":
		trans = buildMuxTransport(globalDocUrl, func(u string) transport.Transport {
			return yandex.NewBoardsTransport(u, config)
		})
	case "direct":
		// Обычный TCP до ноды. Не скрытый канал: адрес ноды виден и режется
		// тривиально — зато он доступен, когда носитель с документом упёрся в
		// капчу, и по нему можно доставить ноде куки. Шифрование обязательно.
		if *directAddr == "" {
			log.Fatalf("--transport=direct requires --direct-addr host:port")
		}
		if *encryptionKeyFile == "" {
			log.Fatalf("--transport=direct requires --encryption-key-file (a plain TCP carrier must not run unencrypted)")
		}
		dcfg := transport.DefaultDirectConfig()
		dcfg.IsExit = *exitNode
		if *exitNode {
			dcfg.ListenAddr = *directAddr
		} else {
			dcfg.DialAddr = *directAddr
		}
		trans = transport.NewAdaptiveTransport(transport.NewDirectTransport(config, dcfg))
	case "yandex":
		trans = buildMuxTransport(globalDocUrl, func(u string) transport.Transport {
			return newYandexDocs(u, config)
		})
	case "vyandex", "volga":
		trans = buildMuxTransport(globalDocUrl, func(u string) transport.Transport {
			return yandex.NewYandexVolgaTransport(u, config)
		})
	case "mailru", "mail":
		trans = buildMuxTransport(globalDocUrl, func(u string) transport.Transport {
			return mailru.NewMailruDocsTransport(u, config)
		})
	case "oneme":
		uidint, _ := strconv.ParseInt(maxUid, 10, 64)
		trans = transport.NewCompressedTransport(oneme.NewOneMeTransport(*exitNode, maxToken, uidint, config))
	default:
		log.Fatalf("Unknown transport type: %s", *transportType)
	}

	// Optional AES-256-GCM, outermost — the same position the upstream CLI uses,
	// so a node built from this branch and one built from upstream master speak
	// the same wire format: each tunnel packet is sealed first, and the codec
	// layer below batches the ciphertext.
	//
	// The context string is a public KDF salt, not a secret — but both peers must
	// derive from the SAME one, and upstream derives it from the document URL.
	// Keep that rule identical here: a differing URL silently yields a different
	// key and every packet is dropped as unauthenticated.
	if *encryptionKeyFile != "" {
		secretBytes, err := os.ReadFile(*encryptionKeyFile)
		if err != nil {
			log.Fatalf("Read encryption key file: %v", err)
		}
		// Контекст — публичная соль вывода ключа, но обе стороны обязаны взять
		// ОДНУ И ТУ ЖЕ строку. Для доковых транспортов это URL документа, он у
		// клиента и ноды одинаков. Для direct так нельзя: нода слушает
		// 0.0.0.0:9443, а клиент набирает 64.118.154.75:9443 — строки разные, и
		// ключи молча разъехались бы (каждый пакет не проходит аутентификацию,
		// снаружи это выглядит как таймауты). Поэтому у direct контекст — имя
		// транспорта, единственное, в чём стороны заведомо согласны.
		context := *transportType
		if globalDocUrl != "" && *transportType != "direct" {
			context = globalDocUrl
		}
		encrypted, err := transport.NewEncryptedTransport(
			trans, strings.TrimSpace(string(secretBytes)), context, *exitNode)
		if err != nil {
			log.Fatalf("Configure encrypted transport: %v", err)
		}
		trans = encrypted
		log.Printf("Transport encryption: AES-256-GCM enabled (KDF context=%q)", context)
	}

	// Контрольный канал: обмен куками для обхода интерактивной капчи. Оборачивает
	// уже собранный стек, поэтому его кадры проходят через слой шифрования ниже и
	// уезжают зашифрованными наравне с данными туннеля.
	trans = wrapControl(trans, *exitNode)
	if !*exitNode {
		startCookieOffering()
	}

	if err := trans.Start(); err != nil {
		log.Fatalf("Failed to start transport: %v", err)
	}
	// Always-on readiness marker (independent of --debug) so a supervisor or
	// service manager can detect a healthy start by scanning the journal.
	log.Printf("OPENFLUX_READY transport=%s mode=%s", *transportType,
		map[bool]string{true: "exit-node", false: "client"}[*exitNode])

	// Живое подхватывание кук: единственный путь пройти интерактивную капчу на
	// ноде, где нет браузера. Запускаем после Start, чтобы транспорты успели
	// зарегистрироваться.
	if *cookiesFile != "" {
		setCookiesFilePath(*cookiesFile)
		utils.SafeGo("cookiesFileWatch", func() { watchCookiesFile(*cookiesFile) })
	}

	tun := tunnel.NewTCPTunnel(trans, *exitNode)

	if *exitNode {
		log.Printf("Running as EXIT NODE (needs root for raw socket)")
		if *localIP != "" {
			// Scoped: only touch traffic originating from the tunnel's egress IP,
			// leaving the host's other services untouched. The kernel would
			// otherwise reset TCP (RST) and reject inbound UDP replies (ICMP
			// port-unreachable) for the userspace-owned connections.
			log.Printf("! Run: sudo iptables -A OUTPUT -p tcp --tcp-flags RST RST -s %s -j DROP", *localIP)
			log.Printf("!      sudo iptables -A OUTPUT -p icmp --icmp-type port-unreachable -s %s -j DROP", *localIP)
		} else {
			log.Printf("! Kernel RSTs/ICMP would tear down tunnel connections. Prefer scoped rules:")
			log.Printf("!   assign a dedicated alias IP, run with --local-ip <ip>, then:")
			log.Printf("!   sudo iptables -A OUTPUT -p tcp --tcp-flags RST RST -s <ip> -j DROP")
			log.Printf("!   sudo iptables -A OUTPUT -p icmp --icmp-type port-unreachable -s <ip> -j DROP")
			log.Printf("! Host-wide fallback (affects the whole host; closed ports look filtered):")
			log.Printf("!   sudo iptables -A OUTPUT -p tcp --tcp-flags RST RST -j DROP")
			log.Printf("!   sudo iptables -A OUTPUT -p icmp --icmp-type port-unreachable -j DROP")
		}
		select {}
	} else {
		log.Printf("Running as CLIENT (SOCKS5 on %s)", *socksAddr)
		socks5Server := socks5.NewSOCKS5Server(*socksAddr, tun)
		log.Fatal(socks5Server.Start())
	}
}
