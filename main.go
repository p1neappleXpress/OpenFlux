package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"runtime"
	godebug "runtime/debug"
	"strings"
	"time"

	"openflux/internal/transportstack"
	"openflux/socks5"
	"openflux/transport"
	"openflux/tunnel"
	"openflux/utils"
)

var (
	globalDocUrl string
	maxToken     string
	maxUid       string
	localIP      string
)

// expandShortFlags rewrites single-letter flag aliases into their long
// forms so both -r and --role work. Handles bare flags (-d) and inline
// values (-r=exit, -u=https://...).
func expandShortFlags(args []string) []string {
	aliases := map[string]string{
		"-r": "--role",
		"-i": "--inbound",
		"-t": "--transport",
		"-m": "--mode",
		"-c": "--codec",
		"-u": "--url",
		"-s": "--socks5",
		"-l": "--local-ip",
		"-d": "--debug",
	}
	out := make([]string, 0, len(args))
	for _, a := range args {
		replaced := false
		for short, long := range aliases {
			if a == short {
				out = append(out, long)
				replaced = true
				break
			}
			if strings.HasPrefix(a, short+"=") {
				out = append(out, long+a[len(short):])
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, a)
		}
	}
	return out
}

const (
	roleClient    = "client"
	roleExit      = "exit"
	roleBenchSend = "bench-send"
	roleBenchSink = "bench-sink"
)

const (
	inboundTUN    = "tun"
	inboundSOCKS5 = "socks5"
)

const (
	codecBatched = "batched"
	codecLegacy  = "legacy"
)

func main() {
	fmt.Print("written by p1neappleXpress\n")

	role := flag.String("role", roleClient, "client | exit | bench-send | bench-sink")
	inbound := flag.String("inbound", "", "tun | socks5 (client only; default: tun on macOS, socks5 elsewhere)")
	transportType := flag.String("transport", "yandex", "Transport type (yandex, vyandex, oneme, cupsonline, mailru)")
	mode := flag.String("mode", "", "Exit-node mode: l3 (default, Linux only) or l4 (works everywhere)")

	codec := flag.String("codec", codecBatched, "batched (default, zstd+coalescing) or legacy (per-packet LZ4)")
	encryptionKeyFile := flag.String("encryption-key-file", "",
		"Optional: encrypt the transport with AES-256-GCM using a shared secret read from this file. "+
			"Both peers must use the same secret; unset means unencrypted, unchanged behavior")

	flag.StringVar(&globalDocUrl, "url", "http://#", "Document URL. If u use Yandex.Docs transport")
	flag.StringVar(&maxToken, "maxToken", "", "MAX Web token. If u use MAX transport")
	flag.StringVar(&maxUid, "maxUid", "", "MAX call user id. If u use MAX transport")
	socksAddr := flag.String("socks5", ":1080", "SOCKS5 address")
	flag.StringVar(&localIP, "local-ip", "", "Egress IP for exit node (l3 mode only, scoped RST drop)")

	benchBytes := flag.Int("bench-bytes", 0, "Benchmark: push this many MB through the transport, then report and exit")
	benchCompressible := flag.Bool("bench-compressible", false, "Benchmark: use compressible payload instead of random")

	debug := flag.Bool("debug", false, "Enable verbose debug logging")

	// Deprecated aliases, kept for one release to ease migration.
	depClient := flag.Bool("client", false, "DEPRECATED: use --role=client")
	depExit := flag.Bool("exit-node", false, "DEPRECATED: use --role=exit")
	depTun := flag.Bool("tun", false, "DEPRECATED: use --inbound=tun")
	depSocks5Mode := flag.Bool("socks5-mode", false, "DEPRECATED: use --inbound=socks5")
	depLegacy := flag.Bool("legacy", false, "DEPRECATED: use --codec=legacy")
	depBenchSend := flag.Int("bench-send", 0, "DEPRECATED: use --role=bench-send --bench-bytes=N")
	depBenchSink := flag.Bool("bench-sink", false, "DEPRECATED: use --role=bench-sink")

	// Override the default flag.PrintDefaults so -h prints a structured
	// usage message with axes, modifiers, and examples instead of a flat
	// alphabetical list.
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `OpenFlux — Network stack research tool. TCP tunnel with pluggable transports.

USAGE
  openflux --role=<role> --transport=<type> [OPTIONS]

ROLE
  -r, --role=client       Run as client. (default)
  -r, --role=exit         Run as exit node.
  -r, --role=bench-send   Benchmark: push --bench-bytes MB.
  -r, --role=bench-sink   Benchmark: receive from transport.

TRANSPORT
  -t, --transport=yandex       Yandex.Docs over WebSocket. (default)
  -t, --transport=vyandex      Yandex.Volga over HTTP relay + WS.
  -t, --transport=oneme        MAX (VK) over WebRTC.
  -t, --transport=cupsonline   Cups.online interview rooms.
  -t, --transport=mailru       Mail.ru Docs over WebSocket.

  -u, --url=<URL>              Document URL.
      --maxToken=<token>       MAX auth token (--transport=oneme).
      --maxUid=<uid>           MAX user id   (--transport=oneme).

INBOUND  (only with --role=client)
  -i, --inbound=tun            utun (macOS) / NEPacketTunnel (iOS). Default on macOS.
  -i, --inbound=socks5         SOCKS5 + gVisor. Default on other platforms.
  -s, --socks5=<addr>          SOCKS5 listen address (default :1080).

MODE  (only with --role=exit)
  -m, --mode=l3                Packet forwarding (SNAT/DNAT). Default.
  -m, --mode=l4                Stream proxy (TCP termination + re-dial).
  -l, --local-ip=<ip>          Egress IP for SNAT. Auto-detected.

TRANSPORT MODIFIERS
  -c, --codec=batched          zstd + coalescing. Default.
  -c, --codec=legacy           Per-packet LZ4. A/B only.
      --encryption-key-file=<path>
                               AES-256-GCM wrapper. Both peers must share the same key.

BENCHMARK  (only with --role=bench-*)
      --bench-bytes=<MB>       MB to push (bench-send).
      --bench-compressible     Repetitive payload (bench-send).

LOGGING
  -d, --debug                  Verbose per-packet logging.

DEPRECATED (removed in v2)
  -client, -exit-node      -> --role=client|exit
  -tun, -socks5-mode       -> --inbound=tun|socks5
  -legacy                  -> --codec=legacy
  -bench-send, -bench-sink -> --role=bench-send|bench-sink
`)
	}

	os.Args = expandShortFlags(os.Args)
	flag.Parse()

	// Map deprecated flags to their new counterparts. New flags win over
	// deprecated ones if both are supplied.
	roleSet := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "role" {
			roleSet = true
		}
	})
	if !roleSet {
		if *depClient {
			log.Printf("warning: -client is deprecated, use --role=client")
			*role = roleClient
		}
		if *depExit {
			log.Printf("warning: -exit-node is deprecated, use --role=exit")
			*role = roleExit
		}
	}
	if *depTun {
		log.Printf("warning: -tun is deprecated, use --inbound=tun")
		*inbound = inboundTUN
	}
	if *depSocks5Mode {
		log.Printf("warning: -socks5-mode is deprecated, use --inbound=socks5")
		*inbound = inboundSOCKS5
	}
	if *depLegacy {
		log.Printf("warning: -legacy is deprecated, use --codec=legacy")
		*codec = codecLegacy
	}
	if *depBenchSend > 0 {
		log.Printf("warning: -bench-send is deprecated, use --role=bench-send --bench-bytes=N")
		*role = roleBenchSend
		*benchBytes = *depBenchSend
	}
	if *depBenchSink {
		log.Printf("warning: -bench-sink is deprecated, use --role=bench-sink")
		*role = roleBenchSink
	}

	// Platform defaults. The recommended client path is utun on macOS and
	// SOCKS5 everywhere else (see README for details).
	if *inbound == "" {
		if runtime.GOOS == "darwin" {
			*inbound = inboundTUN
		} else {
			*inbound = inboundSOCKS5
		}
	}
	if *mode == "" {
		*mode = "l3"
	}

	if *codec != codecBatched && *codec != codecLegacy {
		log.Fatalf("--codec: unknown value %q (want batched|legacy)", *codec)
	}

	switch *role {
	case roleClient:
		if *inbound != inboundTUN && *inbound != inboundSOCKS5 {
			log.Fatalf("--role=client: unknown --inbound=%q (want tun|socks5)", *inbound)
		}
	case roleExit:
		if *mode != "l3" && *mode != "l4" {
			log.Fatalf("--role=exit: unknown --mode=%q (want l3|l4)", *mode)
		}
	case roleBenchSend, roleBenchSink:
		// No ingress or exit mode.
	default:
		log.Fatalf("unknown --role=%q (want client|exit|bench-send|bench-sink)", *role)
	}

	// Warn when the exit runs on l4 (gVisor): it works everywhere but is
	// slower than l3 (SNAT/DNAT, Linux only, needs root + iptables).
	if *role == roleExit && *mode == "l4" {
		log.Printf("warning: exit on l4 (gVisor). l3 is faster on Linux with root.")
	}

	exitMode, err := tunnel.ParseExitMode(*mode)
	if err != nil {
		log.Fatalf("--mode: %v", err)
	}

	// The exit node often runs on a tiny VPS; keep the heap tight under load
	// (GC aggressively). Set GOMEMLIMIT in the environment for a hard soft-cap.
	if *role == roleExit {
		godebug.SetGCPercent(20)
	}

	if *debug {
		utils.EnableDebug()
	}

	log.Printf("=== Universal Bypass Tool ===")
	log.Printf("Role: %s", *role)
	log.Printf("Transport: %s", *transportType)
	if *role == roleClient {
		log.Printf("Inbound: %s", *inbound)
	}
	if *role == roleExit {
		log.Printf("Exit mode: %s", exitMode.String())
	}

	secret := ""
	if *encryptionKeyFile != "" {
		secretBytes, err := os.ReadFile(*encryptionKeyFile)
		if err != nil {
			log.Fatal("Cannot read encryption key file")
		}
		secret = strings.TrimSpace(string(secretBytes))
		clear(secretBytes)
		if secret == "" {
			log.Fatal("Encryption key file is empty")
		}
	}
	trans, err := transportstack.New(transportstack.Options{
		Transport: *transportType, URL: globalDocUrl, MAXToken: maxToken, MAXUID: maxUid,
		Codec: *codec, EncryptionSecret: secret, ExitNode: *role == roleExit,
	})
	if err != nil {
		log.Fatalf("Configure transport: %v", err)
	}
	log.Printf("Codec: %s", *codec)
	if secret != "" {
		log.Printf("Transport encryption: AES-256-GCM enabled")
	}

	// Benchmark modes run the transport directly with no tunnel / raw socket,
	// so they never touch the host network.
	if *role == roleBenchSend {
		if *benchBytes <= 0 {
			log.Fatalf("--role=bench-send requires --bench-bytes=<MB>")
		}
		runBenchSend(trans, *benchBytes, *benchCompressible)
		return
	}
	if *role == roleBenchSink {
		runBenchSink(trans)
		return
	}

	if err := trans.Start(); err != nil {
		log.Fatalf("Failed to start transport: %v", err)
	}

	switch *role {
	case roleExit:
		runExit(trans, exitMode)
	case roleClient:
		runClient(trans, *inbound, *socksAddr, exitMode)
	default:
		log.Fatalf("unhandled role %q", *role)
	}
}

func runExit(trans transport.Transport, exitMode tunnel.ExitMode) {
	ex, err := tunnel.NewExitNode(trans, exitMode.String())
	if err != nil {
		log.Fatalf("exit node: %v", err)
	}
	log.Printf("Running as EXIT NODE (mode=%s)", ex.Mode())
	if err := ex.Start(); err != nil {
		log.Fatalf("exit start: %v", err)
	}

	// L3 SNAT rewrites source IPs; the kernel sees return packets for
	// connections it never opened and emits RST, tearing them down.
	// The operator must drop outbound RSTs matching the egress IP.
	if exitMode == tunnel.ExitModeL3 {
		if localIP != "" {
			log.Printf("! Run: sudo iptables -A OUTPUT -p tcp --tcp-flags RST RST -s %s -j DROP", localIP)
		} else {
			log.Printf("! Kernel RSTs would tear down tunnel connections. Prefer a scoped rule:")
			log.Printf("!   assign a dedicated alias IP, run with --local-ip <ip>, then:")
			log.Printf("!   sudo iptables -A OUTPUT -p tcp --tcp-flags RST RST -s <ip> -j DROP")
			log.Printf("! Host-wide fallback (drops ALL outbound RST; makes closed ports look filtered):")
			log.Printf("!   sudo iptables -A OUTPUT -p tcp --tcp-flags RST RST -j DROP")
		}
	}

	select {}
}

func runClient(trans transport.Transport, inbound, socksAddr string, exitMode tunnel.ExitMode) {
	switch inbound {
	case inboundTUN:
		runClientTUN(trans)
	case inboundSOCKS5:
		// Explicit opt-in to the legacy SOCKS5+gVisor client. Kept as a fallback
		// for platforms without a tun client (see README).
		log.Printf("Running as CLIENT (SOCKS5 on %s, legacy gVisor path)", socksAddr)
		tun := tunnel.NewTCPTunnelMode(trans, false, exitMode)
		socks5Server := socks5.NewSOCKS5Server(socksAddr, tun)
		log.Fatal(socks5Server.Start())
	default:
		log.Fatalf("--inbound: unknown value %q (want tun|socks5)", inbound)
	}
}

func runClientTUN(trans transport.Transport) {
	tc, err := NewTUNClient(trans, 1280)
	if err != nil {
		log.Fatalf("utun: %v", err)
	}
	log.Printf("utun interface: %s", tc.Name())

	// Save the CURRENT default (which may be another VPN's utun) so
	// we can restore it on exit no matter what.
	if err := tc.SaveDefault(); err != nil {
		log.Fatalf("save default route: %v", err)
	}
	if err := tc.SetupInterface(); err != nil {
		log.Fatalf("setup utun (need sudo): %v", err)
	}
	log.Printf("utun up; bypass gateway is %s", tc.Gateway())

	watcher := NewSocketWatcher(tc.Gateway(), func() {
		log.Printf("Socket set stable; taking default route into the tunnel")
		if err := tc.ConfigureDefault(); err != nil {
			log.Printf("FATAL: configure default: %v", err)
			return
		}
		tc.Start()
		log.Printf("Tunnel active")
	})
	watcher.Start(2 * time.Second)

	sigCh := make(chan os.Signal, 1)
	notifySignals(sigCh)
	<-sigCh
	watcher.Stop()
	log.Printf("Shutting down, restoring default route...")
	if err := tc.Close(); err != nil {
		log.Printf("cleanup warning: %v", err)
	}
	tc.RestoreDefault()
	log.Printf("Shutdown complete")
	os.Exit(0)
}
