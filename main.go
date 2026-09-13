package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	godebug "runtime/debug"
	"strconv"
	"strings"

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
	flag.StringVar(&globalDocUrl, "url", "http://#", "Document URL. If u use Yandex.Docs transport")
	flag.StringVar(&maxToken, "maxToken", "", "MAX Web token. If u use MAX transport")
	flag.StringVar(&maxUid, "maxUid", "", "MAX call user id. If u use MAX transport")
	flag.StringVar(&localIP, "local-ip", "", "Egress IP for exit node (raw mode only, scoped RST drop)")
	encryptionKeyFile := flag.String("encryption-key-file", "",
		"Optional: encrypt the transport with AES-256-GCM using a shared secret read from this file. "+
			"Both peers must use the same secret; unset means unencrypted, unchanged behavior")
	legacy := flag.Bool("legacy", false, "Use legacy per-packet LZ4 codec without batching (for A/B comparison)")
	benchSend := flag.Int("bench-send", 0, "Benchmark: push this many MB through the transport, then report and exit")
	benchSink := flag.Bool("bench-sink", false, "Benchmark: receive from the transport and measure goodput")
	benchCompressible := flag.Bool("bench-compressible", false, "Benchmark: use compressible payload instead of random")
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

	benchMode := *benchSink || *benchSend > 0
	if !*exitNode && !*client && !benchMode {
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
	switch *transportType {
	case "vyandex":
		inner = yandex.NewYandexVolgaTransport(globalDocUrl, config)
	case "yandex":
		inner = yandex.NewYandexDocsTransport(globalDocUrl, config)
	case "oneme":
		uidint, _ := strconv.ParseInt(maxUid, 10, 64)
		inner = oneme.NewOneMeTransport(*exitNode, maxToken, uidint, config)
	case "cupsonline":
		inner = cupsonline.NewCupsonlineTransport(globalDocUrl, config, !*exitNode)
	default:
		log.Fatalf("Unknown transport type: %s", *transportType)
	}

	// Optional AES-256-GCM encryption sits closest to the raw transport, so on
	// send we batch/compress first and encrypt the result (ciphertext would not
	// compress). Both peers must use the same secret.
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

	// App-layer codec, outermost. Default is the new batching+zstd layer;
	// --legacy selects the old per-packet LZ4 path so the two can be compared
	// over the same channel. Client and exit node must use the same one.
	var trans transport.Transport
	if *legacy {
		log.Printf("Codec: legacy (per-packet LZ4, no batching)")
		trans = transport.NewCompressedTransport(inner)
	} else {
		log.Printf("Codec: batched (zstd + coalescing)")
		trans = transport.NewBatchedTransport(inner)
	}

	// Benchmark modes run the transport directly with no tunnel / raw socket,
	// so they never touch the host network.
	if *benchSink {
		runBenchSink(trans)
		return
	}
	if *benchSend > 0 {
		runBenchSend(trans, *benchSend, *benchCompressible)
		return
	}

	if err := trans.Start(); err != nil {
		log.Fatalf("Failed to start transport: %v", err)
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
