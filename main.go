package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	godebug "runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	fluxcore "fluxcore"

	"universal-bypass-tool/socks5"
	"universal-bypass-tool/transport"
	"universal-bypass-tool/transport/cupsonline"
	"universal-bypass-tool/transport/oneme"
	"universal-bypass-tool/transport/wss"
	"universal-bypass-tool/transport/yandex"
	"universal-bypass-tool/tunnel"
	"universal-bypass-tool/utils"
)

var version = "0.2.0-dev"

type legacyOptions struct {
	exitNode          bool
	socksAddr         string
	transportType     string
	exitMode          tunnel.ExitMode
	documentURL       string
	maxToken          string
	maxUID            string
	localIP           string
	encryptionKeyFile string
}

func main() {
	if err := runCLI(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		log.Printf("OpenFlux: %v", err)
		os.Exit(1)
	}
}

func runCLI(args []string) error {
	flags := flag.NewFlagSet("openflux", flag.ContinueOnError)
	exitNode := flags.Bool("exit-node", false, "Run as exit node")
	client := flags.Bool("client", false, "Run as client")
	debug := flags.Bool("debug", false, "Enable verbose debug logging")
	configPath := flags.String("config", "", "Path to an OpenFlux JSON configuration")
	showVersion := flags.Bool("version", false, "Print version and exit")
	socksAddr := flags.String("socks5", "127.0.0.1:1080", "SOCKS5 listen address")
	transportType := flags.String("transport", "yandex", "Legacy transport (yandex, vyandex, oneme, cupsonline)")
	mode := flags.String("mode", "proxy", "Legacy exit-node mode: proxy or raw")
	documentURL := flags.String("url", "http://#", "Document URL for a legacy transport")
	maxToken := flags.String("maxToken", "", "MAX Web token for the experimental OneMe transport")
	maxUID := flags.String("maxUid", "", "MAX call user ID for the experimental OneMe transport")
	localIP := flags.String("local-ip", "", "Egress IP for legacy raw exit mode")
	encryptionKeyFile := flags.String(
		"encryption-key-file",
		"",
		"Encrypt a legacy packet transport with a shared secret read from this file",
	)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *showVersion {
		fmt.Printf("OpenFlux %s\n", version)
		return nil
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %s", strings.Join(flags.Args(), " "))
	}

	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	fmt.Printf("OpenFlux %s (written by p1neappleXpress)\n", version)
	if *configPath != "" {
		if *client || *exitNode {
			return errors.New("--config cannot be combined with --client or --exit-node; set mode in the configuration")
		}
		return runConfigured(ctx, *configPath, *debug)
	}
	if *client == *exitNode {
		return errors.New("exactly one of --client or --exit-node is required")
	}
	if *debug {
		utils.EnableDebug()
	}

	exitMode, err := tunnel.ParseExitMode(*mode)
	if err != nil {
		return fmt.Errorf("--mode: %w", err)
	}
	return runLegacy(ctx, legacyOptions{
		exitNode:          *exitNode,
		socksAddr:         *socksAddr,
		transportType:     *transportType,
		exitMode:          exitMode,
		documentURL:       *documentURL,
		maxToken:          *maxToken,
		maxUID:            *maxUID,
		localIP:           *localIP,
		encryptionKeyFile: *encryptionKeyFile,
	})
}

func runConfigured(ctx context.Context, configPath string, forceDebug bool) error {
	config, err := fluxcore.LoadConfig(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	config.ExpandEnvVars()
	if err := config.Validate(); err != nil {
		return fmt.Errorf("validate config: %w", err)
	}
	if config.Debug || forceDebug {
		utils.EnableDebug()
	}

	route, index, ok := firstEnabledRoute(config.Routes)
	if !ok {
		return errors.New("configuration contains no enabled route")
	}
	route = resolveWSSFilePaths(configPath, route)

	if config.Mode == "exit-node" {
		godebug.SetGCPercent(20)
	}
	log.Printf("=== OpenFlux ===")
	log.Printf("Mode: %s", strings.ToUpper(config.Mode))
	log.Printf("Route: %d (%s)", index, route.Transport)

	if route.Transport == "wss" {
		return runWSSRoute(ctx, config, route)
	}

	log.Printf("WARNING: %s uses the experimental legacy packet/gVisor path", route.Transport)
	return runLegacy(ctx, legacyOptions{
		exitNode:      config.Mode == "exit-node",
		socksAddr:     config.SOCKS5,
		transportType: route.Transport,
		exitMode:      tunnel.ExitModeProxy,
		documentURL:   route.URL,
		maxToken:      route.MaxToken,
		maxUID:        route.MaxUID,
	})
}

func firstEnabledRoute(routes []fluxcore.RouteConfig) (fluxcore.RouteConfig, int, bool) {
	for index, route := range routes {
		if !route.Disabled {
			return route, index, true
		}
	}
	return fluxcore.RouteConfig{}, -1, false
}

func resolveWSSFilePaths(configPath string, route fluxcore.RouteConfig) fluxcore.RouteConfig {
	if route.Transport != "wss" {
		return route
	}
	baseDirectory := filepath.Dir(configPath)
	resolve := func(path string) string {
		if path == "" || filepath.IsAbs(path) {
			return path
		}
		return filepath.Join(baseDirectory, path)
	}
	route.CAFile = resolve(route.CAFile)
	route.CertFile = resolve(route.CertFile)
	route.KeyFile = resolve(route.KeyFile)
	return route
}

func runWSSRoute(ctx context.Context, config fluxcore.Config, route fluxcore.RouteConfig) error {
	if config.Mode == "client" {
		client, err := wss.NewClient(wss.ClientConfig{
			Endpoint:   route.Endpoint,
			AuthToken:  route.AuthToken,
			CAFile:     route.CAFile,
			ServerName: route.ServerName,
			ProxyURL:   route.ProxyURL,
		})
		if err != nil {
			return fmt.Errorf("configure WSS client: %w", err)
		}
		server := socks5.NewSOCKS5Server(config.SOCKS5, client)
		if err := server.Bind(); err != nil {
			return fmt.Errorf("bind SOCKS5 listener %s: %w", config.SOCKS5, err)
		}
		log.Printf("SOCKS5 listening on %s", config.SOCKS5)
		log.Printf("WSS endpoint: %s", route.Endpoint)
		return serveSOCKSUntilCanceled(ctx, server)
	}

	server, err := wss.NewServer(wss.ServerConfig{
		ListenAddr: route.ListenAddr,
		CertFile:   route.CertFile,
		KeyFile:    route.KeyFile,
		AuthToken:  route.AuthToken,
		DenyCIDRs:  route.DenyCIDRs,
	})
	if err != nil {
		return fmt.Errorf("configure WSS exit: %w", err)
	}
	listener, err := net.Listen("tcp", route.ListenAddr)
	if err != nil {
		return fmt.Errorf("bind WSS listener %s: %w", route.ListenAddr, err)
	}
	log.Printf("WSS exit listening on %s", listener.Addr())
	return serveWSSUntilCanceled(ctx, server, listener)
}

func serveSOCKSUntilCanceled(ctx context.Context, server *socks5.SOCKS5Server) error {
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.Start()
	}()

	select {
	case err := <-serveDone:
		closeErr := server.Close()
		if ctx.Err() != nil {
			return errors.Join(closeErr, normalizeServerError(err))
		}
		if err == nil {
			err = errors.New("SOCKS5 server stopped unexpectedly")
		}
		return errors.Join(err, closeErr)
	case <-ctx.Done():
		closeErr := server.Close()
		serveErr := normalizeServerError(<-serveDone)
		return errors.Join(closeErr, serveErr)
	}
}

func serveWSSUntilCanceled(ctx context.Context, server *wss.Server, listener net.Listener) error {
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.Serve(listener)
	}()

	shutdown := func() error {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdownContext)
	}

	select {
	case err := <-serveDone:
		shutdownErr := shutdown()
		if ctx.Err() != nil {
			return errors.Join(shutdownErr, normalizeServerError(err))
		}
		if err == nil {
			err = errors.New("WSS exit server stopped unexpectedly")
		}
		return errors.Join(err, shutdownErr)
	case <-ctx.Done():
		shutdownErr := shutdown()
		serveErr := normalizeServerError(<-serveDone)
		return errors.Join(shutdownErr, serveErr)
	}
}

func normalizeServerError(err error) error {
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

func runLegacy(ctx context.Context, options legacyOptions) (runErr error) {
	if options.exitNode {
		godebug.SetGCPercent(20)
	}
	if options.exitNode && options.exitMode == tunnel.ExitModeRaw && options.localIP != "" {
		tunnel.SetLocalIP(options.localIP)
	}

	log.Printf("=== OpenFlux legacy mode (experimental) ===")
	log.Printf("Mode: %s", map[bool]string{true: "EXIT NODE", false: "CLIENT"}[options.exitNode])
	log.Printf("Transport: %s", options.transportType)
	if options.exitNode {
		log.Printf("Exit mode: %s", options.exitMode.String())
	}

	transportConfig := transport.DefaultConfig()
	var inner transport.Transport
	switch options.transportType {
	case "vyandex":
		inner = yandex.NewYandexVolgaTransport(options.documentURL, transportConfig)
	case "yandex":
		inner = yandex.NewYandexDocsTransport(options.documentURL, transportConfig)
	case "oneme":
		var uid int64
		if strings.TrimSpace(options.maxUID) != "" {
			parsedUID, err := strconv.ParseInt(strings.TrimSpace(options.maxUID), 10, 64)
			if err != nil || parsedUID <= 0 {
				return errors.New("--maxUid must be a positive integer")
			}
			uid = parsedUID
		} else if !options.exitNode {
			return errors.New("--maxUid is required for an OneMe client")
		}
		inner = oneme.NewOneMeTransport(options.exitNode, options.maxToken, uid, transportConfig)
	case "cupsonline":
		inner = cupsonline.NewCupsonlineTransport(options.documentURL, transportConfig, !options.exitNode)
	default:
		return fmt.Errorf("unknown legacy transport %q", options.transportType)
	}

	if options.encryptionKeyFile != "" {
		secretBytes, err := os.ReadFile(options.encryptionKeyFile)
		if err != nil {
			return fmt.Errorf("read encryption key file: %w", err)
		}
		keyContext := options.transportType
		if options.documentURL != "" {
			keyContext = options.documentURL
		}
		encrypted, err := transport.NewEncryptedTransport(
			inner,
			strings.TrimSpace(string(secretBytes)),
			keyContext,
			options.exitNode,
		)
		if err != nil {
			return fmt.Errorf("configure encrypted transport: %w", err)
		}
		inner = encrypted
		log.Printf("Transport encryption: AES-256-GCM enabled")
	}

	trans := transport.NewCompressedTransport(inner)
	if err := trans.Start(); err != nil {
		return fmt.Errorf("start legacy transport: %w", err)
	}
	defer func() {
		runErr = errors.Join(runErr, trans.Stop())
	}()

	tcpTunnel := tunnel.NewTCPTunnelMode(trans, options.exitNode, options.exitMode)
	if options.exitNode {
		if options.exitMode == tunnel.ExitModeRaw {
			log.Printf("Running experimental raw exit mode; no firewall changes are made by OpenFlux")
		} else {
			log.Printf("Running experimental proxy exit mode")
		}
		<-ctx.Done()
		return nil
	}

	server := socks5.NewSOCKS5Server(options.socksAddr, tcpTunnel)
	if err := server.Bind(); err != nil {
		return fmt.Errorf("bind SOCKS5 listener %s: %w", options.socksAddr, err)
	}
	log.Printf("SOCKS5 listening on %s", options.socksAddr)
	return serveSOCKSUntilCanceled(ctx, server)
}
