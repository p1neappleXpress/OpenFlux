package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"runtime"
	godebug "runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/p1neappleXpress/OpenFlux/netbind"
	"github.com/p1neappleXpress/OpenFlux/socks5"
	"github.com/p1neappleXpress/OpenFlux/streamproxy"
	"github.com/p1neappleXpress/OpenFlux/transport"
	"github.com/p1neappleXpress/OpenFlux/transport/control"
	"github.com/p1neappleXpress/OpenFlux/transport/cupsonline"
	"github.com/p1neappleXpress/OpenFlux/transport/ipc"
	"github.com/p1neappleXpress/OpenFlux/transport/mailru"
	"github.com/p1neappleXpress/OpenFlux/transport/manager"
	"github.com/p1neappleXpress/OpenFlux/transport/oneme"
	"github.com/p1neappleXpress/OpenFlux/transport/phpbox"
	"github.com/p1neappleXpress/OpenFlux/transport/yandex"
	"github.com/p1neappleXpress/OpenFlux/tunnel"
	"github.com/p1neappleXpress/OpenFlux/utils"
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
	}
	out := make([]string, 0, len(args))
	for i, a := range args {
		// Counted debug flag: -d, -dd, -ddd -> --debug=1..3. Only a run of
		// d's counts, so single-dash long flags like -direct-listen pass.
		if n := debugCount(a); n > 0 {
			out = append(out, fmt.Sprintf("--debug=%d", min(n, utils.LevelHexdump)))
			continue
		}
		if strings.HasPrefix(a, "-d=") {
			out = append(out, "--debug="+a[len("-d="):])
			continue
		}
		// A bare --debug means -d, unless its level follows ("--debug 2").
		if a == "--debug" || a == "-debug" {
			if i+1 >= len(args) || !isNumber(args[i+1]) {
				out = append(out, "--debug=1")
				continue
			}
		}
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

// transportHasCookies reports whether the given transport uses HTTP cookies
// that can be refreshed via the NegotiatedTransport control channel.
func transportHasCookies(t string) bool {
	switch t {
	case "yandex", "vyandex", "boards", "mailru", "cupsonline":
		return true
	}
	return false
}

// cookieKey identifies a session inside the cookie store. For most transports
// this is the document URL; for oneme it would be maxUid, but oneme does not
// use the store at all.
func cookieKey(transportType, docURL, maxUid string) string {
	switch transportType {
	case "oneme":
		return maxUid
	default:
		return docURL
	}
}

// debugCount returns how many d's make up a -d, -dd, -ddd flag, or 0.
func debugCount(a string) int {
	if len(a) < 2 || a[0] != '-' || strings.Trim(a[1:], "d") != "" {
		return 0
	}
	return len(a) - 1
}

func isNumber(s string) bool {
	_, err := strconv.Atoi(s)
	return err == nil
}

// contextSources describes the carriers for transport.KDFContexts, the one
// rule every peer (CLI, Android, Desktop, iOS) derives the encryption
// context with.
func contextSources(specs []transportSpec) []transport.ContextSource {
	out := make([]transport.ContextSource, len(specs))
	for i, s := range specs {
		out[i] = transport.ContextSource{Type: s.Type, URL: s.URL, Priority: s.Priority}
	}
	return out
}

// managerRefreshLoop periodically asks the exit node for a fresh cookie jar.
// Runs on the client side only, when --transports or --negotiate is set.
func managerRefreshLoop(m *manager.Manager) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		if !m.IsConnected() {
			continue
		}
		if err := m.SendControl(control.SubtypeCookiesRequest, nil); err != nil {
			utils.Debugf("[CTRL] cookies request failed: %v", err)
			continue
		}
		utils.Debugf("[CTRL] cookies request sent")
	}
}

func main() {
	// The desktop wizard's JSON protocol owns stdout: no banner, no flags.
	if len(os.Args) == 2 && os.Args[1] == "--node-wizard" {
		os.Exit(runNodeWizard(os.Stdin, os.Stdout))
	}
	// The core's own openflux:// parser and builder for apps and scripts,
	// so a link is read and made the same way everywhere: JSON on stdout,
	// before any banner.
	if len(os.Args) == 3 && os.Args[1] == "--parse-link" {
		os.Exit(runParseLink(os.Args[2], os.Stdin, os.Stdout))
	}
	if len(os.Args) == 3 && os.Args[1] == "--make-link" {
		os.Exit(runMakeLink(os.Args[2], os.Stdin, os.Stdout))
	}
	fmt.Print("written by p1neappleXpress\n")

	role := flag.String("role", roleClient, "client | exit | bench-send | bench-sink")
	inbound := flag.String("inbound", "", "tun | socks5 (client only; default: tun on macOS, socks5 elsewhere)")
	transportType := flag.String("transport", "yandex", "Transport type (yandex, vyandex, oneme, cupsonline, mailru)")
	mode := flag.String("mode", "", "Exit-node mode: l3 (default; Linux as root, or Windows as Administrator with WinDivert) or l4 (works everywhere)")

	codec := flag.String("codec", codecBatched, "batched (default, zstd+coalescing) or legacy (per-packet LZ4)")
	negotiate := flag.Bool("negotiate", false, "Require encrypted, session-bound IPv4 capability negotiation on both peers (no legacy fallback)")
	maxPacket := flag.Int("max-packet-size", transport.MaxNegotiatedPacket, "Maximum IPv4 packet in negotiated mode (1280..65000); not the Internet path MTU")
	cookieStorePath := flag.String("cookie-store", "",
		"Path to the cookie jar file. Default: ./cookies-<transport>.json in the current directory. "+
			"Ignored for transports without cookies (direct, oneme).")
	encryptionKeyFile := flag.String("encryption-key-file", "",
		"Optional: encrypt the transport with AES-256-GCM using a shared secret read from this file. "+
			"Both peers must use the same secret; unset means unencrypted, unchanged behavior")
	sessionContextFlag := flag.String("session-context", "",
		"Explicit KDF context for --encryption-key-file. Both peers must use the same value. "+
			"Default: derived from the document URL (--url, any --<type>-url, or [Transport] URL "+
			"from --config), falling back to --transport. Only set this to override that derivation.")

	flag.StringVar(&globalDocUrl, "url", "http://#", "Document URL. If u use Yandex.Docs transport")
	flag.StringVar(&maxToken, "maxToken", "", "MAX Web token. If u use MAX transport")
	flag.StringVar(&maxUid, "maxUid", "", "MAX call user id. If u use MAX transport")
	directDial := flag.String("direct-dial", "", "DirectTransport: exit address to dial (client). Requires --encryption-key-file")
	directListen := flag.String("direct-listen", "", "DirectTransport: local address to listen on (exit). Requires --encryption-key-file")
	transportsFlag := flag.String("transports", "",
		"Comma-separated list of transports with priorities, e.g. "+
			"\"direct:100,yandex:50\". If empty, --transport is used as a single transport.")
	yandexURL := flag.String("yandex-url", "", "URL for the yandex transport (overrides --url in --transports mode)")
	vyandexURL := flag.String("vyandex-url", "", "URL for the vyandex transport")
	flag.StringVar(&yandexCookiesFile, "yandex-cookies-file", "", "Netscape cookies.txt with a Yandex login for vyandex transports")
	boardsURL := flag.String("boards-url", "", "URL for the boards transport")
	mailruURL := flag.String("mailru-url", "", "URL (weblink) for the mailru transport")
	cupsonlineURL := flag.String("cupsonline-url", "", "URL for the cupsonline transport")
	onemeToken := flag.String("oneme-token", "", "MAX token for the oneme transport")
	onemeUID := flag.String("oneme-uid", "", "MAX uid for the oneme transport")
	configPath := flag.String("config", "",
		"Path to an OpenFlux .conf file. Command-line flags override values from the file.")
	shareFlag := flag.Bool("share", false,
		"Exit: print an openflux:// link and QR code that clients scan to connect (contains the encryption key)")
	shareHost := flag.String("share-host", "",
		"Exit: address clients dial for direct in the --share link. Default: this host's first public IPv4")
	ipcSocketPath := flag.String("ipc-socket", "",
		"Path to the Unix domain socket used by the mobile app to talk to the core. "+
			"Empty = no IPC server.")
	socksAddr := flag.String("socks5", ":1080", "SOCKS5 address")
	httpProxyAddr := flag.String("http-proxy", "", "Client: also serve an HTTP proxy (CONNECT and plain requests) on this address, through the same tunnel")
	flag.StringVar(&localIP, "local-ip", "", "Egress IP for exit node (l3 mode only, scoped RST drop)")

	benchBytes := flag.Int("bench-bytes", 0, "Benchmark: push this many MB through the transport, then report and exit")
	benchCompressible := flag.Bool("bench-compressible", false, "Benchmark: use compressible payload instead of random")

	debug := flag.Int("debug", 0, "Debug level: 1 packets (-d), 2 operational logs (-dd), 3 hexdumps (-ddd)")
	sensitive := flag.Bool("sensitive", false, "Also log key material and, with -ddd, plaintext frames (cookie jars, tokens)")
	sensitiveAlias := flag.Bool("sensetive", false, "Alias for --sensitive")

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
  openflux --role=<role> --transports=<list> [OPTIONS]
  openflux --config=/path/to/openflux.conf [OPTIONS]

ROLE
  -r, --role=client       Run as client. (default)
  -r, --role=exit         Run as exit node.
  -r, --role=bench-send   Benchmark: push --bench-bytes MB.
  -r, --role=bench-sink   Benchmark: receive from transport.

TRANSPORT  (single-transport mode)
  -t, --transport=yandex       Yandex.Docs over WebSocket. (default)
  -t, --transport=vyandex      Yandex.Volga over HTTP relay + WS.
  -t, --transport=oneme        MAX (VK) over WebRTC.
  -t, --transport=cupsonline   Cups.online interview rooms.
  -t, --transport=mailru       Mail.ru Docs over WebSocket.
  -t, --transport=direct       Plain TCP to a self-hosted exit.
  -u, --url=<URL>              Document URL.

TRANSPORTS  (multi-transport session; requires --encryption-key-file)
      --transports=direct:100,yandex:50
                               Comma-separated list of transports with
                               priorities. Higher priority = tried first
                               for the handshake and for control traffic.
                               All listed transports are attached to one
                               Session; IPv4 flows are hashed across them.
      --yandex-url=<URL>       URL for the yandex transport.
      --vyandex-url=<URL>      URL for the vyandex transport.
      --yandex-cookies-file=<path>
                               Netscape cookies.txt with a Yandex login for
                               vyandex transports.
      --boards-url=<URL>       URL for the boards transport.
      --mailru-url=<WEBLINK>   Weblink for the mailru transport.
      --cupsonline-url=<URL>   URL for the cupsonline transport.
      --oneme-token=<token>    MAX auth token for the oneme transport.
      --oneme-uid=<uid>        MAX user id for the oneme transport.
      --direct-dial=<addr>     DirectTransport: exit host:port (client).
      --direct-listen=<addr>   DirectTransport: listen addr on exit.

INBOUND  (only with --role=client)
  -i, --inbound=tun            utun (macOS) / Wintun (Windows, needs administrator
                               and wintun.dll next to the binary) / NEPacketTunnel
                               (iOS). Default on macOS.
  -i, --inbound=socks5         SOCKS5 + gVisor. Default on other platforms.
  -s, --socks5=<addr>          SOCKS5 listen address (default :1080).
      --http-proxy=<addr>      Also serve an HTTP proxy (CONNECT and plain
                               requests) on this address, e.g. for a system
                               proxy that only speaks HTTP.

MODE  (only with --role=exit)
  -m, --mode=l3                Packet forwarding (SNAT/DNAT). Default. Linux
                               as root; Windows as Administrator with
                               WinDivert.dll + WinDivert64.sys beside the core.
  -m, --mode=l4                Stream proxy (TCP termination + re-dial).
  -l, --local-ip=<ip>          Egress IP for SNAT. Auto-detected.

TRANSPORT MODIFIERS
  -c, --codec=batched          zstd + coalescing. Default.
  -c, --codec=legacy           Per-packet LZ4. A/B only.
      --encryption-key-file=<path>
                               AES-256-GCM wrapper. Required with
                               --transports or --negotiate. Both peers
                               must share the same key.
      --session-context=<str>  Explicit KDF context for that key. Both peers
                               must use the same value. Default: --url, else
                               the URL of the highest-priority transport,
                               else "http://#".
      --negotiate              Require authenticated capability negotiation
                               on both peers. No legacy fallback.
      --max-packet-size=N      Max IPv4 packet in negotiated mode
                               (1280..65000). Default 65000.
      --cookie-store=<path>    Cookie jar file. Default: ./cookies-<transport>.json.
      --ipc-socket=<path>      Unix domain socket for the mobile bridge.
                               Empty = no IPC server.
      --share                  Exit: print an openflux:// link and QR code for
                               clients to scan. Contains the encryption key.
      --share-host=<host>      Exit: address clients dial for direct in the
                               link. Default: this host's first public IPv4.
      --config=<path>          Load settings from an OpenFlux .conf file
                               (INI-like, similar to wg-quick). Command-line
                               flags override values from the file.

BENCHMARK  (only with --role=bench-*)
      --bench-bytes=<MB>       MB to push (bench-send).
      --bench-compressible     Repetitive payload (bench-send).

LOGGING
  -d, --debug=1                Packet movement: one line per IPv4 packet,
                               "-> 52 bytes - UDP 10.10.10.2:53000 -> 8.8.8.8:53 ...".
  -dd, --debug=2               Plus operational logs: sessions, carriers,
                               handshakes, crypto, control, errors.
  -ddd, --debug=3              Plus hexdumps of packets and ciphertext.
      --sensitive              Also log key material and, with -ddd, the
                               plaintext frames (control messages carry
                               cookie jars and tokens). Off by default.

DEPRECATED (removed in v2)
  -client, -exit-node      -> --role=client|exit
  -tun, -socks5-mode       -> --inbound=tun|socks5
  -legacy                  -> --codec=legacy
  -bench-send, -bench-sink -> --role=bench-send|bench-sink
`)
	}

	os.Args = expandShortFlags(os.Args)
	flag.Parse()

	// isExit is the session/encryption-directionality role: which side of a
	// negotiated or encrypted pair this process plays. bench-sink is the
	// passive, always-listening side (like an exit), bench-send the active
	// initiator (like a client) - without this, two bench processes both
	// resolved to "client", so a negotiated session never had an exit side
	// to answer hellos with a challenge, and two peers' encryption keys
	// were derived in the same direction instead of swapped.
	isExit := *role == roleExit || *role == roleBenchSink

	// Apply .conf file if requested. Only flags that were not explicitly set
	// on the command line are overridden.
	//
	// confTransports is declared at function scope (not inside the if) so
	// pickSessionContext can see it below: a .conf-only deployment has no
	// --url/--yandex-url and its document URL lives in the [Transport]
	// sections, which must still contribute to the KDF context.
	var confTransports []transportSpec
	if *configPath != "" {
		conf, err := parseConf(*configPath)
		if err != nil {
			log.Fatalf("--config: %v", err)
		}
		setFlags := make(map[string]bool)
		flag.Visit(func(f *flag.Flag) { setFlags[f.Name] = true })

		applyConfString(conf.Interface, "Role", "role", role, setFlags)
		// Role may have just changed: isExit above was computed from the
		// pre-.conf value, so a config-only "Role = exit" (no --role on the
		// command line, as every node-wizard deployment runs) left isExit
		// stuck at false. Recompute before it's used below.
		isExit = *role == roleExit || *role == roleBenchSink
		applyConfString(conf.Interface, "Inbound", "inbound", inbound, setFlags)
		applyConfString(conf.Interface, "Transport", "transport", transportType, setFlags)
		applyConfString(conf.Interface, "Mode", "mode", mode, setFlags)
		applyConfString(conf.Interface, "Codec", "codec", codec, setFlags)
		applyConfString(conf.Interface, "Socks5", "socks5", socksAddr, setFlags)
		applyConfString(conf.Interface, "EncryptionKeyFile", "encryption-key-file", encryptionKeyFile, setFlags)
		applyConfString(conf.Interface, "SessionContext", "session-context", sessionContextFlag, setFlags)
		applyConfString(conf.Interface, "CookieStore", "cookie-store", cookieStorePath, setFlags)
		applyConfString(conf.Interface, "IPCSocket", "ipc-socket", ipcSocketPath, setFlags)
		applyConfString(conf.Interface, "URL", "url", &globalDocUrl, setFlags)
		if v, ok := confValue(conf.Interface, "Debug"); ok && !setFlags["debug"] {
			if b, err := strconv.Atoi(v); err == nil {
				*debug = b
			} else if confBool(v, false) {
				*debug = 1
			}
		}
		if v, ok := confValue(conf.Interface, "Sensitive"); ok && !setFlags["sensitive"] && !setFlags["sensetive"] {
			*sensitive = confBool(v, *sensitive)
		}

		for _, t := range conf.Transports {
			if t.Name == "" {
				continue
			}
			spec := transportSpec{
				Name:     t.Name,
				Type:     t.Values["Type"],
				Priority: confInt(t.Values["Priority"], 50),
				URL:      t.Values["URL"],
			}
			if spec.Type == "" {
				spec.Type = t.Name
			}
			if spec.Type == "direct" {
				spec.Params = map[string]interface{}{
					"dial":    t.Values["Dial"],
					"listen":  t.Values["Listen"],
					"is_exit": isExit,
				}
			}
			if spec.Type == "oneme" {
				spec.Params = map[string]interface{}{
					"token": t.Values["Token"],
					"uid":   t.Values["UID"],
					"exit":  isExit,
				}
			}
			confTransports = append(confTransports, spec)
		}
	}

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

	// Windows full tunnel: bind the core's sockets to the real interface
	// before any transport dials, so the carriers stay out of the tunnel
	// once it takes the default route (see package netbind).
	if *role == roleClient && *inbound == inboundTUN && runtime.GOOS == "windows" {
		index, err := netbind.BindDefault()
		if err != nil {
			log.Fatalf("tun: %v", err)
		}
		log.Printf("core sockets bound to interface %d", index)
	}

	// Warn when the exit runs on l4 (gVisor): it works everywhere but is
	// slower than l3 (SNAT/DNAT: Linux as root, Windows with WinDivert).
	if *role == roleExit && *mode == "l4" {
		log.Printf("warning: exit on l4 (gVisor). l3 is faster: Linux as root, Windows as Administrator.")
	}

	// --mode=stream: the client speaks the phpbox stream mux (OPEN/DATA/CLOSE
	// frames) over the transport instead of IP packets through gVisor, and a
	// local SOCKS5 hands each app connection to a mux stream. The exit is a
	// phpbox exit (deploy/phpbox over cups). This is a circuit-level TCP
	// tunnel, not L7 - the exit never parses the application protocol.
	// The debug level is set here, not only further down: this branch returns before that code runs.
	if *role == roleClient && *mode == "stream" {
		utils.SetLevel(*debug)
		if *sensitive || *sensitiveAlias {
			utils.SetSensitive(true)
		}
		if *inbound == inboundTUN {
			runStreamTUN(*transportType, globalDocUrl)
			return
		}
		runStreamClient(*transportType, globalDocUrl, *socksAddr, *httpProxyAddr, *ipcSocketPath)
		return
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

	utils.SetLevel(*debug)
	if *sensitive || *sensitiveAlias {
		utils.SetSensitive(true)
	}
	utils.Debugf("[INIT] debug level=%d sensitive=%v", utils.Level(), utils.Sensitive())

	log.Printf("=== OpenFlux ===")
	log.Printf("Role: %s", *role)
	// A .conf's [Transport ...] sections or --transports run a Session of
	// several carriers; *transportType stays at its flag default ("yandex")
	// in both cases, so printing it unconditionally here always claimed
	// "yandex" regardless of what was actually configured.
	switch {
	case len(confTransports) > 0:
		names := make([]string, len(confTransports))
		for i, t := range confTransports {
			names[i] = t.Type
		}
		log.Printf("Transport: %s (session)", strings.Join(names, ", "))
	case *transportsFlag != "":
		log.Printf("Transport: %s (session)", *transportsFlag)
	default:
		log.Printf("Transport: %s", *transportType)
	}
	if *role == roleClient {
		log.Printf("Inbound: %s", *inbound)
	}
	if *role == roleExit {
		log.Printf("Exit mode: %s", exitMode.String())
	}

	config := transport.DefaultConfig()

	// Cookie store: per-transport file in pwd, unless --cookie-store is set.
	// Transports without cookies (direct, oneme) skip it entirely.
	var store *transport.CookieStore
	if transportHasCookies(*transportType) {
		path := *cookieStorePath
		if path == "" {
			path = fmt.Sprintf("./cookies-%s.json", *transportType)
		}
		s, err := transport.NewCookieStore(path)
		if err != nil {
			log.Fatalf("Cookie store %s: %v", path, err)
		}
		store = s
		log.Printf("Cookie store: %s", path)
	}

	// Build the list of transports to run. Two modes:
	//   --transports=direct:100,yandex:50  -> multi-transport session
	//   --transport=<type>                 -> legacy single-transport mode
	var specs []transportSpec
	// Running transports whose client address (cupsonline rooms) exists only
	// once they start, by spec name, for --share.
	rooms := make(map[string]roomLister)
	if len(confTransports) > 0 {
		specs = confTransports
		// Per-type URL flags still override config values.
		urls := map[string]string{
			"yandex":     *yandexURL,
			"vyandex":    *vyandexURL,
			"boards":     *boardsURL,
			"mailru":     *mailruURL,
			"cupsonline": *cupsonlineURL,
		}
		specs = buildTransportSpecs(specs, urls, nil)
	} else if *transportsFlag != "" {
		parsed, err := parseTransportList(*transportsFlag)
		if err != nil {
			log.Fatalf("--transports: %v", err)
		}
		urls := map[string]string{
			"yandex":     *yandexURL,
			"vyandex":    *vyandexURL,
			"boards":     *boardsURL,
			"mailru":     *mailruURL,
			"cupsonline": *cupsonlineURL,
		}
		if globalDocUrl != "" && globalDocUrl != "http://#" && urls["yandex"] == "" {
			urls["yandex"] = globalDocUrl
		}
		extra := map[string]map[string]interface{}{
			"oneme": {"token": *onemeToken, "uid": *onemeUID, "exit": isExit},
			"direct": {
				"dial":    *directDial,
				"listen":  *directListen,
				"is_exit": isExit,
			},
		}
		specs = buildTransportSpecs(parsed, urls, extra)
	} else {
		// Named after the type, as --transports and the apps name
		// carriers: cookie exchange with a Session peer is by name.
		specs = []transportSpec{{
			Name:     *transportType,
			Type:     *transportType,
			Priority: 100,
			URL:      globalDocUrl,
		}}
		if *transportType == "oneme" {
			specs[0].Params = map[string]interface{}{
				"token": maxToken, "uid": maxUid, "exit": isExit,
			}
		}
		if *transportType == "direct" {
			specs[0].Params = map[string]interface{}{
				"dial": *directDial, "listen": *directListen, "is_exit": isExit,
			}
		}
	}

	// Validate --codec with the multi-transport path. Session always uses
	// BatchedTransport, so --codec=legacy is only valid in single-transport
	// non-negotiated mode.
	if *negotiate && *codec != codecBatched {
		log.Fatal("--negotiate requires --codec=batched")
	}

	// Encryption secret is mandatory when --negotiate is set.
	//
	// The session context is the KDF salt for the encryption keys and MUST
	// be identical on both peers; see pickSessionContext.
	var secret string
	var sessionContext string
	if *encryptionKeyFile != "" {
		b, err := os.ReadFile(*encryptionKeyFile)
		if err != nil {
			log.Fatalf("Read encryption key file: %v", err)
		}
		secret = strings.TrimSpace(string(b))
		if n := utils.SecretChars(secret); n < utils.MinSecretChars {
			log.Fatalf("Encryption key from %s is too short (%d chars, need at least %d)",
				*encryptionKeyFile, n, utils.MinSecretChars)
		}
		if strings.ContainsAny(secret, "\r\n\t") {
			utils.Debugf("[KEY] WARNING: secret still contains whitespace after TrimSpace; lengths may differ across platforms")
		}
		// No hash of the secret without --sensitive: it would let anyone
		// with the log test guesses without paying for scrypt.
		utils.Debugf("[KEY] loaded from %s: len=%d", *encryptionKeyFile, len(secret))
		if utils.Sensitive() {
			utils.Debugf("[KEY] secret sha256=%s", utils.Sha256Hex([]byte(secret)))
		}
	}

	sessionContext, contextAlternates := transport.KDFContexts(*sessionContextFlag, globalDocUrl, contextSources(specs))
	if *encryptionKeyFile != "" {
		utils.Debugf("[KEY] context=%q sha256=%s (MUST match on both peers; %d alternates tried on mismatch)",
			sessionContext, utils.Sha256Hex([]byte(sessionContext)), len(contextAlternates))
		log.Printf("Encryption context: sha256 %s", utils.Sha256Short([]byte(sessionContext)))
	}

	// Decide whether we run the full Session path (encryption + negotiate)
	// or the legacy single-transport path.
	var (
		managerInst *manager.Manager
		trans       transport.Transport
		exchanger   transport.CookieExchanger
		demux       *transport.PortDemux
	)

	// configuredSession: the operator asked for a Session. [Transport]
	// sections in a .conf describe one just like --transports.
	//
	// A classic setup (--transport=X) with a key runs as a Session too,
	// with classic compatibility: a client falls back to the classic
	// layering while the exit does not answer the handshake and upgrades
	// once it does; an exit serves classic clients and Session clients.
	// Only --negotiate is strict. Without a key only classic is possible.
	configuredSession := *negotiate || *transportsFlag != "" || len(confTransports) > 0
	classicCompat := false
	switch {
	case *role != roleClient && *role != roleExit:
	case !configuredSession && secret != "":
		classicCompat = true
	case configuredSession && !*negotiate && *role == roleClient && len(specs) == 1:
		classicCompat = true
	}
	if configuredSession || classicCompat {
		if secret == "" {
			log.Fatal("--transports/--negotiate/.conf transports require --encryption-key-file")
		}
		switch {
		case classicCompat && isExit:
			log.Printf("Mode: Session, also serving classic clients (--negotiate makes it Session-only)")
		case classicCompat:
			log.Printf("Mode: Session, falling back to classic while the exit does not answer the handshake")
		default:
			log.Printf("Mode: Session")
		}

		caps := transport.CapabilityIPv4 | transport.CapabilityTCP | transport.CapabilityUDP
		if *role == roleClient || exitMode == tunnel.ExitModeL3 {
			caps |= transport.CapabilityICMPErrors
		}
		params := transport.PeerParameters{
			Capabilities:  caps,
			MaxPacketSize: *maxPacket,
		}
		sess, err := transport.NewSession(params, isExit)
		if err != nil {
			log.Fatal(err)
		}
		if classicCompat {
			sess.SetClassic(*codec)
		}
		sess.SetAlternateContexts(contextAlternates)

		// Build the factory that SubtypeTransportStart will use for
		// dynamic transports.
		factory := transportFactory(config, isExit)
		managerInst = manager.New(sess, factory, secret, sessionContext)

		if err := registerBootstrapTransports(managerInst, specs, config, secret, sessionContext, rooms); err != nil {
			log.Fatalf("bootstrap transports: %v", err)
		}

		// Persist each cookie-carrying transport's jar and replay what was
		// saved; the Manager routes cookie control messages by name.
		if store != nil {
			for _, spec := range specs {
				if !transportHasCookies(spec.Type) {
					continue
				}
				if err := managerInst.UseCookieStore(store, spec.Name, cookieKey(spec.Type, spec.URL, maxUid)); err != nil {
					utils.Debugf("[COOKIE] replay %s: %v", spec.Name, err)
				}
			}
		}

		// Hook the Session control dispatcher into the manager.
		sess.SetControlHandler(managerInst.DispatchControl)

		// Optional IPC bridge: if --ipc-socket is set, the core talks to the
		// mobile app over a Unix domain socket. Transport-initiated captcha
		// requests go out as MsgCookiesRequest; cookies offers come in as
		// MsgCookiesOffer.
		if *ipcSocketPath != "" {
			h := &coreIPCHandler{manager: managerInst}
			srv := ipc.NewServer(*ipcSocketPath, h)
			if err := srv.Listen(); err != nil {
				log.Fatalf("IPC listen %s: %v", *ipcSocketPath, err)
			}
			defer srv.Close()
			statusServer = srv

			// Checks for local transports go to the app as-is; checks the
			// exit reports are marked Remote, to be passed from its address.
			managerInst.SetCaptchaNotifier(func(name, url, reason string) {
				_ = srv.SendCookiesRequest(&ipc.CookiesRequestPayload{
					Transport: name, URL: url, Reason: reason,
				})
			})
			if *role == roleClient {
				demux = transport.NewPortDemux(managerInst, authProxyPortLo, authProxyPortHi)
				authProxy := &remoteAuthProxy{demux: demux}
				managerInst.SetRemoteAuthNotifier(func(name, url, reason string) {
					proxy, err := authProxy.Addr()
					if err != nil {
						log.Printf("remote auth proxy: %v", err)
					}
					_ = srv.SendCookiesRequest(&ipc.CookiesRequestPayload{
						Transport: name, URL: url, Reason: reason, Remote: true, Proxy: proxy,
					})
				})
			}
		}

		trans = managerInst
		if demux != nil {
			trans = demux
		}
		exchanger = nil // cookie handling lives in the Manager

	} else {
		// Classic single-transport path without a Session: no key (the
		// Session needs one), or a bench role.
		var inner transport.Transport
		switch *transportType {
		case "boards":
			inner = yandex.NewBoardsTransport(globalDocUrl, config)
		case "vyandex":
			t, err := newVolgaTransport(globalDocUrl, config)
			if err != nil {
				log.Fatalf("vyandex: %v", err)
			}
			inner = t
		case "yandex":
			inner = yandex.NewYandexDocsTransport(globalDocUrl, config)
		case "oneme":
			uidint, _ := strconv.ParseInt(maxUid, 10, 64)
			inner = oneme.NewOneMeTransport(isExit, maxToken, uidint, config)
		case "cupsonline":
			c := cupsonline.NewCupsonlineTransport(globalDocUrl, config, !isExit)
			rooms[specs[0].Name] = c
			inner = c
		case "mailru":
			inner = mailru.NewMailruDocsTransport(globalDocUrl, config)
		default:
			log.Fatalf("Unknown transport type: %s", *transportType)
		}

		// Persist cookie exchanger for the legacy path.
		if store != nil {
			if ce, ok := inner.(transport.CookieExchanger); ok {
				key := cookieKey(*transportType, globalDocUrl, maxUid)
				if jar := store.Load(key); jar != nil {
					_ = ce.ApplyCookies(jar)
				}
				exchanger = transport.NewPersistentCookieExchanger(ce, store, key)
			}
		}

		// Either framing is accepted; the preferred one is sent until the
		// peer shows which it speaks (see transport.CodecTransport).
		log.Printf("Codec: %s preferred, falls back to the other framing when the peer does not answer", *codec)
		inner = transport.NewCodecTransport(inner, *codec, !isExit)

		if *encryptionKeyFile != "" {
			encrypted, err := transport.NewEncryptedTransport(inner, secret, sessionContext, isExit)
			if err != nil {
				log.Fatalf("Configure encrypted transport: %v", err)
			}
			encrypted.SetAlternateContexts(contextAlternates)
			inner = encrypted
			log.Printf("Transport encryption: AES-256-GCM enabled")
		} else {
			log.Printf("Transport encryption: OFF (no --encryption-key-file): the carrier sees the traffic, and a peer with a key cannot talk to this one")
		}

		trans = inner

		// The app's IPC bridge works here too: traffic totals for its speed
		// counters, and cookies it offers go to the carrier.
		if *ipcSocketPath != "" {
			srv := ipc.NewServer(*ipcSocketPath, &coreIPCHandler{exchanger: exchanger})
			if err := srv.Listen(); err != nil {
				log.Fatalf("IPC listen %s: %v", *ipcSocketPath, err)
			}
			defer srv.Close()
			statusServer = srv
		}
	}

	_ = exchanger
	_ = managerInst

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

	if statusServer != nil {
		if managerInst != nil {
			utils.SafeGo("ipc-status", func() { ipcStatusLoop(statusServer, managerInst, managerInst.Session().ActiveTransport) })
		} else {
			utils.SafeGo("ipc-status", func() { ipcStatusLoop(statusServer, trans, nil) })
		}
	}

	// Periodically ask the exit node to refresh its cookies. Only the client
	// initiates; the exit answers with SubtypeCookiesResponse.
	if *role == roleClient && managerInst != nil {
		utils.SafeGo("cookie-refresh", func() { managerRefreshLoop(managerInst) })
	}
	if managerInst != nil {
		if pp, ready := managerInst.Session().PeerParameters(); ready {
			log.Printf("Authenticated peer: IPv4 TCP; UDP=%t; ICMP errors=%t; maximum packet=%d",
				pp.Capabilities&transport.CapabilityUDP != 0,
				pp.Capabilities&transport.CapabilityICMPErrors != 0,
				pp.MaxPacketSize)
		}
	} else if n, ok := trans.(*transport.Session); ok {
		if pp, ready := n.PeerParameters(); ready {
			log.Printf("Authenticated peer: IPv4 TCP; UDP=%t; ICMP errors=%t; maximum packet=%d",
				pp.Capabilities&transport.CapabilityUDP != 0,
				pp.Capabilities&transport.CapabilityICMPErrors != 0,
				pp.MaxPacketSize)
		}
	}

	switch *role {
	case roleExit:
		var printLink func()
		if *shareFlag {
			// A classic exit keeps advertising classic: older clients
			// read the link too, and updated ones upgrade on their own.
			session := configuredSession
			host := *shareHost
			if host == "" {
				host = publicIPv4()
			}
			printLink = func() {
				printShare(shareConfig(specs, session, *codec, secret, sessionContext, host, rooms))
			}
		}
		// A cupsonline exit learns its room list only once it runs. Older
		// classic clients derived their key from that list; accept it as
		// an alternate context. Rooms created later (a start that failed
		// and was retried) or anew change the link: print it again.
		var sess *transport.Session
		if managerInst != nil {
			sess = managerInst.Session()
		}
		for _, r := range rooms {
			r.OnRoomList(func(packed string) {
				if sess != nil && packed != "" {
					sess.SetAlternateContexts([]string{packed})
				}
				if printLink != nil {
					printLink()
				}
			})
			if list := r.RoomList(); list != "" && sess != nil {
				sess.SetAlternateContexts([]string{list})
			}
		}
		if printLink != nil {
			printLink()
		}
		runExit(trans, exitMode)
	case roleClient:
		runClient(trans, *inbound, *socksAddr, *httpProxyAddr, exitMode)
	default:
		log.Fatalf("unhandled role %q", *role)
	}
}

// statusServer is the IPC bridge, set when --ipc-socket is given.
var statusServer *ipc.Server

// statusSource is what the IPC status reports on: a Session's manager or
// a classic carrier without one.
type statusSource interface {
	IsConnected() bool
	Stats() transport.TransportStats
}

// ipcStatusLoop reports to the app every second: whether a carrier reaches
// the peer, traffic totals, uptime and (Sessions) the active carrier.
func ipcStatusLoop(srv *ipc.Server, src statusSource, active func() string) {
	started := time.Now()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for range tick.C {
		st := src.Stats()
		p := &ipc.StatusPayload{
			Running:   true,
			Connected: src.IsConnected(),
			BytesIn:   st.BytesReceived,
			BytesOut:  st.BytesSent,
			UptimeMs:  time.Since(started).Milliseconds(),
		}
		if active != nil {
			p.Active = active()
		}
		_ = srv.SendStatus(p)
	}
}

func runExit(trans transport.Transport, exitMode tunnel.ExitMode) {
	if exitMode == tunnel.ExitModeL3 {
		if err := tunnel.SetLocalIP(localIP); err != nil {
			log.Fatalf("--local-ip: %v", err)
		}
	}
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
	// On Linux the operator must drop outbound RSTs matching the egress IP;
	// the Windows backend drops them itself, per flow, through WinDivert.
	if exitMode == tunnel.ExitModeL3 && runtime.GOOS == "linux" {
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

// runStreamClient runs the --mode=stream client: a raw transport carries the
// phpbox stream mux, and a local SOCKS5 (and optional HTTP) proxy dials each
// app connection out as a mux stream to the phpbox exit. No gVisor, no IP
// packets. The proxying itself is package streamproxy, shared with the
// mobile bridges.
// streamCarrier builds the carrier the stream mux rides.
func streamCarrier(transportType, url string) phpbox.Carrier {
	cfg := transport.DefaultConfig()
	switch transportType {
	case "cupsonline":
		return cupsonline.NewCupsonlineTransport(url, cfg, true)
	case "yandex", "":
		return yandex.NewYandexDocsTransport(url, cfg)
	case "vyandex":
		t, err := newVolgaTransport(url, cfg)
		if err != nil {
			log.Fatalf("--mode=stream vyandex: %v", err)
		}
		return t
	case "mailru":
		return mailru.NewMailruDocsTransport(url, cfg)
	}
	log.Fatalf("--mode=stream: transport %q not supported (use cupsonline, yandex, vyandex, mailru)", transportType)
	return nil
}

// runStreamTUN is the stream mode as a full tunnel (--inbound=tun): the
// system's traffic goes into a local stack that opens one mux stream per TCP
// connection (tunnel.StreamNet, which is a transport, so the utun/Wintun
// client runs on it as it does on any other).
func runStreamTUN(transportType, url string) {
	sn := tunnel.NewStreamNet(streamCarrier(transportType, url))
	if err := sn.Start(); err != nil {
		log.Fatalf("--mode=stream: %v", err)
	}
	log.Printf("Running as CLIENT (stream mux over %s, full tunnel)", transportType)
	runClientTUN(sn)
}

func runStreamClient(transportType, url, socksAddr, httpProxyAddr, ipcSocket string) {
	carrier := streamCarrier(transportType, url)
	p, err := streamproxy.Start(streamproxy.Options{Carrier: carrier, Socks: socksAddr, HTTP: httpProxyAddr, Label: transportType})
	if err != nil {
		log.Fatalf("--mode=stream: %v", err)
	}
	defer p.Stop()

	// The app's IPC bridge works here too: traffic totals for its speed counters.
	if ipcSocket != "" {
		var exchanger transport.CookieExchanger
		if x, ok := carrier.(transport.CookieExchanger); ok {
			exchanger = x
		}
		srv := ipc.NewServer(ipcSocket, &coreIPCHandler{exchanger: exchanger})
		if err := srv.Listen(); err != nil {
			log.Fatalf("IPC listen %s: %v", ipcSocket, err)
		}
		defer srv.Close()
		statusServer = srv
		go streamStatusLoop(srv, p)
	}

	if httpProxyAddr != "" {
		log.Printf("HTTP proxy on %s", httpProxyAddr)
	}
	log.Printf("Running as CLIENT (stream mux over %s, SOCKS5 on %s)", transportType, socksAddr)
	select {}
}

// streamStatusLoop reports the stream client to the app every second.
func streamStatusLoop(srv *ipc.Server, p *streamproxy.Proxy) {
	started := time.Now()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for range tick.C {
		_ = srv.SendStatus(&ipc.StatusPayload{
			Running:   true,
			Connected: p.Connected(),
			BytesIn:   uint64(p.BytesReceived()),
			BytesOut:  uint64(p.BytesSent()),
			UptimeMs:  time.Since(started).Milliseconds(),
		})
	}
}

func runClient(trans transport.Transport, inbound, socksAddr, httpProxyAddr string, exitMode tunnel.ExitMode) {
	switch inbound {
	case inboundTUN:
		runClientTUN(trans)
	case inboundSOCKS5:
		// Explicit opt-in to the legacy SOCKS5+gVisor client. Kept as a fallback
		// for platforms without a tun client (see README).
		log.Printf("Running as CLIENT (SOCKS5 on %s, legacy gVisor path)", socksAddr)
		tun := tunnel.NewTCPTunnelMode(trans, false, exitMode)
		if httpProxyAddr != "" {
			ln, err := net.Listen("tcp", httpProxyAddr)
			if err != nil {
				log.Fatalf("--http-proxy %s: %v", httpProxyAddr, err)
			}
			log.Printf("HTTP proxy on %s", httpProxyAddr)
			utils.SafeGo("http-proxy", func() { _ = tunnel.ServeHTTPProxy(ln, tun.DialTCP) })
		}
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
