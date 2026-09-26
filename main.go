package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"runtime"
	godebug "runtime/debug"
	"strconv"
	"strings"
	"time"

	"openflux/socks5"
	"openflux/transport"
	"openflux/transport/control"
	"openflux/transport/cupsonline"
	"openflux/transport/ipc"
	"openflux/transport/mailru"
	"openflux/transport/manager"
	"openflux/transport/oneme"
	"openflux/transport/yandex"
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

// pickSessionContext returns the KDF salt used to derive encryption keys.
// The same value must be produced on both peers, regardless of how the
// document URL was supplied (--url, --yandex-url, [Transport] URL, ...).
//
// Priority:
//
//	explicit          --session-context, if non-empty
//	--url             globalURL, if non-empty and not the placeholder
//	--<type>-url      first non-empty per-transport URL flag
//	[Transport] URL   first non-empty URL from a .conf section
//	fallback          transportType, or "openflux" if that is empty too
//
// The placeholder "http://#" (the default value of --url) is treated as
// "not set" so it never becomes part of the KDF input.
func pickSessionContext(
	explicit, globalURL string,
	yandexURL, vyandexURL, boardsURL, mailruURL, cupsonlineURL string,
	confTransports []transportSpec,
	transportType string,
) string {
	const placeholder = "http://#"
	if explicit != "" {
		return explicit
	}
	if globalURL != "" && globalURL != placeholder {
		return globalURL
	}
	for _, u := range []string{yandexURL, vyandexURL, boardsURL, mailruURL, cupsonlineURL} {
		if u != "" {
			return u
		}
	}
	for _, s := range confTransports {
		if s.URL != "" {
			return s.URL
		}
	}
	if transportType != "" {
		return transportType
	}
	return "openflux"
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
	fmt.Print("written by p1neappleXpress\n")

	role := flag.String("role", roleClient, "client | exit | bench-send | bench-sink")
	inbound := flag.String("inbound", "", "tun | socks5 (client only; default: tun on macOS, socks5 elsewhere)")
	transportType := flag.String("transport", "yandex", "Transport type (yandex, vyandex, oneme, cupsonline, mailru)")
	mode := flag.String("mode", "", "Exit-node mode: l3 (default, Linux only) or l4 (works everywhere)")

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
      --boards-url=<URL>       URL for the boards transport.
      --mailru-url=<WEBLINK>   Weblink for the mailru transport.
      --cupsonline-url=<URL>   URL for the cupsonline transport.
      --oneme-token=<token>    MAX auth token for the oneme transport.
      --oneme-uid=<uid>        MAX user id for the oneme transport.
      --direct-dial=<addr>     DirectTransport: exit host:port (client).
      --direct-listen=<addr>   DirectTransport: listen addr on exit.

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
                               AES-256-GCM wrapper. Required with
                               --transports or --negotiate. Both peers
                               must share the same key.
      --session-context=<str>  Explicit KDF context for that key. Both peers
                               must use the same value. Default: derived from
                               the document URL, falling back to --transport.
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
					"is_exit": *role == roleExit,
				}
			}
			if spec.Type == "oneme" {
				spec.Params = map[string]interface{}{
					"token": t.Values["Token"],
					"uid":   t.Values["UID"],
					"exit":  *role == roleExit,
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

	utils.SetLevel(*debug)
	if *sensitive || *sensitiveAlias {
		utils.SetSensitive(true)
	}
	utils.Debugf("[INIT] debug level=%d sensitive=%v", utils.Level(), utils.Sensitive())

	log.Printf("=== Universal Bypass Tool ===")
	log.Printf("Role: %s", *role)
	log.Printf("Transport: %s", *transportType)
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
			"oneme": {"token": *onemeToken, "uid": *onemeUID, "exit": *role == roleExit},
			"direct": {
				"dial":    *directDial,
				"listen":  *directListen,
				"is_exit": *role == roleExit,
			},
		}
		specs = buildTransportSpecs(parsed, urls, extra)
	} else {
		specs = []transportSpec{{
			Name:     "primary",
			Type:     *transportType,
			Priority: 100,
			URL:      globalDocUrl,
		}}
		if *transportType == "oneme" {
			specs[0].Params = map[string]interface{}{
				"token": maxToken, "uid": maxUid, "exit": *role == roleExit,
			}
		}
		if *transportType == "direct" {
			specs[0].Params = map[string]interface{}{
				"dial": *directDial, "listen": *directListen, "is_exit": *role == roleExit,
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
		if len(secret) < 16 {
			log.Fatalf("Encryption key from %s is too short (%d chars, need at least 16)",
				*encryptionKeyFile, len(secret))
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

	sessionContext = pickSessionContext(
		*sessionContextFlag,
		globalDocUrl,
		*yandexURL, *vyandexURL, *boardsURL, *mailruURL, *cupsonlineURL,
		confTransports,
		*transportType,
	)
	if *encryptionKeyFile != "" {
		utils.Debugf("[KEY] context=%q sha256=%s (MUST match on both peers)",
			sessionContext, utils.Sha256Hex([]byte(sessionContext)))
	}

	// Decide whether we run the full Session path (encryption + negotiate)
	// or the legacy single-transport path.
	var (
		managerInst *manager.Manager
		trans       transport.Transport
		exchanger   transport.CookieExchanger
		demux       *transport.PortDemux
	)

	// [Transport] sections in a .conf describe a multi-transport session
	// just like --transports; without this they were silently ignored and
	// only the single --transport ran.
	if *negotiate || *transportsFlag != "" || len(confTransports) > 0 {
		if secret == "" {
			log.Fatal("--transports/--negotiate/.conf transports require --encryption-key-file")
		}

		caps := transport.CapabilityIPv4 | transport.CapabilityTCP | transport.CapabilityUDP
		if *role == roleClient || exitMode == tunnel.ExitModeL3 {
			caps |= transport.CapabilityICMPErrors
		}
		params := transport.PeerParameters{
			Capabilities:  caps,
			MaxPacketSize: *maxPacket,
		}
		sess, err := transport.NewSession(params, *role == roleExit)
		if err != nil {
			log.Fatal(err)
		}

		// Build the factory that SubtypeTransportStart will use for
		// dynamic transports.
		factory := transportFactory(config)
		managerInst = manager.New(sess, factory, secret, sessionContext)

		if err := registerBootstrapTransports(managerInst, specs, config, secret, sessionContext); err != nil {
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
		// Legacy single-transport path (no negotiate, no multi).
		var inner transport.Transport
		switch *transportType {
		case "boards":
			inner = yandex.NewBoardsTransport(globalDocUrl, config)
		case "vyandex":
			inner = yandex.NewYandexVolgaTransport(globalDocUrl, config)
		case "yandex":
			inner = yandex.NewYandexDocsTransport(globalDocUrl, config)
		case "oneme":
			uidint, _ := strconv.ParseInt(maxUid, 10, 64)
			inner = oneme.NewOneMeTransport(*role == roleExit, maxToken, uidint, config)
		case "cupsonline":
			inner = cupsonline.NewCupsonlineTransport(globalDocUrl, config, *role != roleExit)
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

		switch *codec {
		case codecBatched:
			log.Printf("Codec: batched (zstd + coalescing)")
			inner = transport.NewBatchedTransport(inner)
		case codecLegacy:
			log.Printf("Codec: legacy (per-packet LZ4, no batching)")
			inner = transport.NewCompressedTransport(inner)
		}

		if *encryptionKeyFile != "" {
			encrypted, err := transport.NewEncryptedTransport(inner, secret, sessionContext, *role == roleExit)
			if err != nil {
				log.Fatalf("Configure encrypted transport: %v", err)
			}
			inner = encrypted
			log.Printf("Transport encryption: AES-256-GCM enabled")
		}

		trans = inner
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
		if *shareFlag {
			session := *negotiate || *transportsFlag != "" || len(confTransports) > 0
			host := *shareHost
			if host == "" {
				host = publicIPv4()
			}
			printShare(shareConfig(specs, session, *codec, secret, sessionContext, host))
		}
		runExit(trans, exitMode)
	case roleClient:
		runClient(trans, *inbound, *socksAddr, exitMode)
	default:
		log.Fatalf("unhandled role %q", *role)
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
