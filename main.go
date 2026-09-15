package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	godebug "runtime/debug"
	"strconv"
	"strings"
	"time"

	"universal-bypass-tool/socks5"
	"universal-bypass-tool/transport"
	"universal-bypass-tool/transport/cupsonline"
	"universal-bypass-tool/transport/oneme"
	"universal-bypass-tool/transport/yandex"
	"universal-bypass-tool/tunnel"
	"universal-bypass-tool/utils"
)

var (
	globalDocUrl string
	maxToken     string
	maxUid       string
	localIP      string
)

func main() {
	fmt.Print("written by p1neappleXpress\n")

	exitNode := flag.Bool("exit-node", false, "Run as exit node")
	client := flag.Bool("client", false, "Run as client")
	debug := flag.Bool("debug", false, "Enable verbose debug logging")
	socksAddr := flag.String("socks5", ":1080", "SOCKS5 address")
	transportType := flag.String("transport", "yandex", "Transport type (yandex, vyandex, oneme, cupsonline)")
	mode := flag.String("mode", "proxy", "Exit-node mode: proxy (default, works everywhere) or raw (Linux only, needs root)")
	flag.StringVar(&globalDocUrl, "url", "http://#",
		"Document URL(s) for Yandex.Docs transport. Comma-separated list enables multi-stream (issue #50): "+
			"the tunnel fans traffic out over N documents round-robin and survives any single one going dead. "+
			"A plain single URL keeps the legacy single-channel behavior.")
	flag.StringVar(&maxToken, "maxToken", "", "MAX Web token. If u use MAX transport")
	flag.StringVar(&maxUid, "maxUid", "", "MAX call user id. If u use MAX transport")
	flag.StringVar(&localIP, "local-ip", "", "Egress IP for exit node (raw mode only, scoped RST drop)")
	encryptionKeyFile := flag.String("encryption-key-file", "",
		"Optional: encrypt the transport with AES-256-GCM using a shared secret read from this file. "+
			"Both peers must use the same secret; unset means unencrypted, unchanged behavior")
	statusEvery := flag.Duration("multistream-status", 0,
		"If >0 and --url has multiple URLs, log per-stream connection state on this interval (e.g. 5s). "+
			"No-op with a single URL.")
	flag.Parse()

	exitMode, err := tunnel.ParseExitMode(*mode)
	if err != nil {
		log.Fatalf("--mode: %v", err)
	}

	if *exitNode && exitMode == tunnel.ExitModeRaw && localIP != "" {
		tunnel.SetLocalIP(localIP)
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
	if *exitNode {
		log.Printf("Exit mode: %s", exitMode.String())
	}

	config := transport.DefaultConfig()
	var inner transport.Transport

	// Split --url on commas so a single flag can carry N documents. Only used
	// for Yandex-family transports here; cupsonline already carries multi-room
	// state inside its base64-encoded URL, and oneme has its own auth path.
	yandexURLs := splitURLs(globalDocUrl)

	switch *transportType {
	case "vyandex":
		inner = buildYandexInner(yandexURLs, config, true)
	case "yandex":
		inner = buildYandexInner(yandexURLs, config, false)
	case "oneme":
		uidint, _ := strconv.ParseInt(maxUid, 10, 64)
		inner = oneme.NewOneMeTransport(*exitNode, maxToken, uidint, config)
	case "cupsonline":
		if len(yandexURLs) > 1 {
			log.Fatalf("cupsonline: multi-URL at CLI level is not supported (already multi-room); pass one base64 URL")
		}
		inner = cupsonline.NewCupsonlineTransport(globalDocUrl, config, !*exitNode)
	default:
		log.Fatalf("Unknown transport type: %s", *transportType)
	}

	if *encryptionKeyFile != "" {
		secretBytes, err := os.ReadFile(*encryptionKeyFile)
		if err != nil {
			log.Fatalf("Read encryption key file: %v", err)
		}
		context := *transportType
		if globalDocUrl != "" {
			context = globalDocUrl
		}
		encrypted, err := transport.NewEncryptedTransport(inner, strings.TrimSpace(string(secretBytes)), context, *exitNode)
		if err != nil {
			log.Fatalf("Configure encrypted transport: %v", err)
		}
		inner = encrypted
		log.Printf("Transport encryption: AES-256-GCM enabled")
	}

	trans := transport.NewCompressedTransport(inner)

	if err := trans.Start(); err != nil {
		log.Fatalf("Failed to start transport: %v", err)
	}

	// Optional per-stream status logger for multi-stream setups. No-op when
	// --url was a single URL (inner is not a MultiStreamTransport in that
	// case).
	if ms, ok := inner.(*transport.MultiStreamTransport); ok && *statusEvery > 0 {
		go multistreamStatusLoop(ms, *statusEvery)
	}

	tun := tunnel.NewTCPTunnelMode(trans, *exitNode, exitMode)

	if *exitNode {
		if exitMode == tunnel.ExitModeRaw {
			log.Printf("Running as EXIT NODE (raw mode)")
			if localIP != "" {
				log.Printf("! Run: sudo iptables -A OUTPUT -p tcp --tcp-flags RST RST -s %s -j DROP", localIP)
			} else {
				log.Printf("! Kernel RSTs would tear down tunnel connections. Prefer a scoped rule:")
				log.Printf("!   assign a dedicated alias IP, run with --local-ip <ip>, then:")
				log.Printf("!   sudo iptables -A OUTPUT -p tcp --tcp-flags RST RST -s <ip> -j DROP")
				log.Printf("! Host-wide fallback (drops ALL outbound RST; makes closed ports look filtered):")
				log.Printf("!   sudo iptables -A OUTPUT -p tcp --tcp-flags RST RST -j DROP")
			}
		} else {
			log.Printf("Running as EXIT NODE (proxy mode)")
		}
		select {}
	} else {
		log.Printf("Running as CLIENT (SOCKS5 on %s)", *socksAddr)
		socks5Server := socks5.NewSOCKS5Server(*socksAddr, tun)
		log.Fatal(socks5Server.Start())
	}
}

// splitURLs splits a comma-separated --url flag, trimming whitespace and
// dropping empties. A plain single URL yields a length-1 slice; a fully-empty
// value yields a length-1 slice with an empty string so callers can pass
// urls[0] without a nil-slice check (the transport itself will fail cleanly).
func splitURLs(raw string) []string {
	if raw == "" {
		return []string{""}
	}
	parts := strings.Split(raw, ",")
	out := parts[:0]
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return []string{""}
	}
	return out
}

// buildYandexInner returns either a single YandexDocsTransport (N=1, exactly
// the legacy behavior) or a MultiStreamTransport fanning out over N of them.
// useVolga picks the alternate engine.io-based vyandex path.
func buildYandexInner(urls []string, config transport.TransportConfig, useVolga bool) transport.Transport {
	mk := func(u string) transport.Transport {
		if useVolga {
			return yandex.NewYandexVolgaTransport(u, config)
		}
		return yandex.NewYandexDocsTransport(u, config)
	}
	if len(urls) <= 1 {
		return mk(urls[0])
	}
	inners := make([]transport.Transport, 0, len(urls))
	for _, u := range urls {
		inners = append(inners, mk(u))
	}
	log.Printf("Multi-stream: %d documents", len(urls))
	return transport.NewMultiStreamTransport(inners)
}

// multistreamStatusLoop prints one status line per interval, useful for
// watching a multi-stream setup during tests and stability checks.
func multistreamStatusLoop(ms *transport.MultiStreamTransport, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for range tick.C {
		streams := ms.Streams()
		parts := make([]string, 0, len(streams))
		up := 0
		for i, s := range streams {
			st := s.Stats()
			state := "DOWN"
			if st.Connected {
				state = "UP"
				up++
			}
			parts = append(parts, fmt.Sprintf("s%d=%s(rx=%d,tx=%d,rc=%d)",
				i, state, st.PacketsRecv, st.PacketsSent, st.Reconnects))
		}
		log.Printf("[MULTI] up=%d/%d %s", up, len(streams), strings.Join(parts, " "))
	}
}
