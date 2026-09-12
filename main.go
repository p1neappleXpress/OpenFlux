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
	ifaceName    string
	mtuOverride  int
)

func main() {
	fmt.Print("written by p1neappleXpress\n")

	exitNode := flag.Bool("exit-node", false, "Run as exit node")
	client := flag.Bool("client", false, "Run as client")
	debug := flag.Bool("debug", false, "Enable verbose debug logging")
	socksAddr := flag.String("socks5", ":1080", "SOCKS5 address")
	transportType := flag.String("transport", "yandex", "Transport type (yandex, vyandex, oneme, cupsonline)")
	flag.StringVar(&globalDocUrl, "url", "http://#",
		"Yandex doc URL, or (cupsonline client) base64 room list, or ignored (cupsonline exit-node)")
	flag.StringVar(&maxToken, "maxToken", "", "MAX Web token. If u use MAX transport")
	flag.StringVar(&maxUid, "maxUid", "", "MAX call user id. If u use MAX transport")
	flag.StringVar(&ifaceName, "iface", "",
		"Interface for client egress (e.g. wg0, eth0). Picks MTU from it. Empty = auto.")
	flag.IntVar(&mtuOverride, "mtu", 0,
		"Override client-interface MTU, in bytes. Set on the CLIENT if a tunnel/VPN with MTU<1500 is in the path. 0 = auto.")
	encryptionKeyFile := flag.String("encryption-key-file", "",
		"Optional: encrypt the transport with AES-256-GCM using a shared secret read from this file. "+
			"Both peers must use the same secret; unset means unencrypted, unchanged behavior")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: %s [--exit-node|--client] [options]

Modes:
  --exit-node   Run as the exit node (proxy mode, no raw sockets).
  --client      Run as the client (SOCKS5 proxy on the local machine).

MTU / interface (client side, optional):

  The CLIENT generates TCP segments, so its MTU decides how big those
  segments are. If a tunnel/VPN (WireGuard, OpenVPN, etc.) sits between
  the client and the exit node with MTU < 1500, set the MTU on the CLIENT,
  otherwise it emits ~1452-byte segments that get dropped and nothing works.

  Priority: --mtu overrides --iface's MTU. If neither is set, MTU is
  auto-detected from the route to 8.8.8.8, falling back to 1500.

Transports:
  yandex       Yandex.Docs document (--url)
  vyandex      Yandex Volga (--url)
  oneme        MAX messenger (--maxToken, --maxUid)
  cupsonline   Cups.online interview rooms
                 exit-node: --url ignored, prints base64 room list on start
                 client:    --url is that base64 room list

Options:
`, os.Args[0])
		flag.PrintDefaults()
	}

	flag.Parse()

	if ifaceName != "" {
		if err := tunnel.SetInterface(ifaceName); err != nil {
			log.Fatalf("Select interface: %v", err)
		}
	}
	if mtuOverride > 0 {
		tunnel.SetMTUOverride(mtuOverride)
	}

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

	tun := tunnel.NewTCPTunnel(trans, *exitNode)

	if *exitNode {
		log.Printf("Running as EXIT NODE (proxy mode)")
		select {}
	} else {
		log.Printf("Running as CLIENT (SOCKS5 on %s)", *socksAddr)
		socks5Server := socks5.NewSOCKS5Server(*socksAddr, tun)
		log.Fatal(socks5Server.Start())
	}
}
