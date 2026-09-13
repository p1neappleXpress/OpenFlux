package fluxcore

import (
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const wssTunnelPath = "/openflux/v1/tunnel"

// RouteConfig declares one path OpenFlux may use. Multiple entries are what
// turns the single-transport tool into a multi-route system (Phase 2 uses the
// list; Phase 1 typically has one).
type RouteConfig struct {
	Transport string `json:"transport"` // "wss" | experimental legacy transport

	// WSS client settings
	Endpoint   string `json:"endpoint,omitempty"`
	AuthToken  string `json:"authToken,omitempty"`
	CAFile     string `json:"caFile,omitempty"`
	ServerName string `json:"serverName,omitempty"`
	ProxyURL   string `json:"proxyURL,omitempty"`

	// WSS exit-node settings
	ListenAddr string   `json:"listenAddr,omitempty"`
	CertFile   string   `json:"certFile,omitempty"`
	KeyFile    string   `json:"keyFile,omitempty"`
	DenyCIDRs  []string `json:"denyCIDRs,omitempty"`

	// Yandex Docs specific
	URL string `json:"url,omitempty"`

	// OneMe/MAX specific
	MaxToken string `json:"maxToken,omitempty"`
	MaxUID   string `json:"maxUid,omitempty"`

	// Reserved for a future direct TCP transport
	DirectAddr string `json:"directAddr,omitempty"`
	UseTLS     bool   `json:"useTLS,omitempty"`

	// Common fields
	Relay  string            `json:"relay,omitempty"`
	Exit   string            `json:"exit,omitempty"`
	Params map[string]string `json:"params,omitempty"`

	// Performance hints (optional)
	Priority int  `json:"priority,omitempty"`
	Disabled bool `json:"disabled,omitempty"`
}

// DNA derives the RouteDNA for this config entry. The WSS endpoint is included
// as non-secret route identity; authentication material is deliberately not.
func (rc RouteConfig) DNA() RouteDNA {
	params := make(map[string]string, len(rc.Params)+1)
	for key, value := range rc.Params {
		params[key] = value
	}
	if rc.Endpoint != "" {
		params["endpoint"] = rc.Endpoint
	}
	return RouteDNA{
		Transport: rc.Transport,
		Relay:     rc.Relay,
		Exit:      rc.Exit,
		Params:    params,
	}
}

// Config is the on-disk configuration for a Flux Core client/daemon.
type Config struct {
	Mode    string `json:"mode"`    // "client" | "exit-node"
	SOCKS5  string `json:"socks5"`  // client SOCKS5 listen addr
	APIAddr string `json:"apiAddr"` // Control API listen addr ("" = off)

	// TUN mode (full device networking)
	TUNMode   bool   `json:"tunMode"`
	TUNIP     string `json:"tunIP,omitempty"`
	TUNDevice string `json:"tunDevice,omitempty"`

	Routes    []RouteConfig `json:"routes"`
	PingEvery Duration      `json:"pingEvery"`
	PingTO    Duration      `json:"pingTimeout"`
	Debug     bool          `json:"debug"`
}

// DefaultConfig returns safe local-only defaults.
func DefaultConfig() Config {
	return Config{
		Mode:      "client",
		SOCKS5:    "127.0.0.1:1080",
		APIAddr:   "127.0.0.1:8787",
		Routes:    []RouteConfig{},
		PingEvery: Duration(3 * time.Second),
		PingTO:    Duration(6 * time.Second),
	}
}

// LoadConfig reads a JSON config file, filling defaults for zero-valued fields.
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, err
	}
	if cfg.PingEvery <= 0 {
		cfg.PingEvery = Duration(3 * time.Second)
	}
	if cfg.PingTO <= 0 {
		cfg.PingTO = Duration(6 * time.Second)
	}
	if cfg.SOCKS5 == "" {
		cfg.SOCKS5 = "127.0.0.1:1080"
	}
	return cfg, nil
}

// Save writes the config as indented JSON.
func (c Config) Save(path string) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// Duration is a JSON-friendly time.Duration that serializes as a string.
type Duration time.Duration

func (d Duration) D() time.Duration { return time.Duration(d) }

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var v interface{}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	switch x := v.(type) {
	case float64:
		*d = Duration(time.Duration(x))
	case string:
		parsed, err := time.ParseDuration(x)
		if err != nil {
			return err
		}
		*d = Duration(parsed)
	default:
		return fmt.Errorf("duration must be a string or number")
	}
	return nil
}

// ExpandEnv replaces ${VAR} and $VAR in strings with environment values.
func ExpandEnv(s string) string {
	return os.ExpandEnv(s)
}

// ExpandEnvVars applies environment expansion to route fields. Secrets should
// normally be referenced from config as ${OPENFLUX_AUTH_TOKEN}, not stored there.
func (c *Config) ExpandEnvVars() {
	for i := range c.Routes {
		r := &c.Routes[i]
		r.Endpoint = ExpandEnv(r.Endpoint)
		r.AuthToken = ExpandEnv(r.AuthToken)
		r.CAFile = ExpandEnv(r.CAFile)
		r.ServerName = ExpandEnv(r.ServerName)
		r.ProxyURL = ExpandEnv(r.ProxyURL)
		r.ListenAddr = ExpandEnv(r.ListenAddr)
		r.CertFile = ExpandEnv(r.CertFile)
		r.KeyFile = ExpandEnv(r.KeyFile)
		for j := range r.DenyCIDRs {
			r.DenyCIDRs[j] = ExpandEnv(r.DenyCIDRs[j])
		}
		r.URL = ExpandEnv(r.URL)
		r.MaxToken = ExpandEnv(r.MaxToken)
		r.MaxUID = ExpandEnv(r.MaxUID)
		r.DirectAddr = ExpandEnv(r.DirectAddr)
	}
}

// Validate checks the config for required fields and logical consistency.
func (c Config) Validate() error {
	if c.Mode != "client" && c.Mode != "exit-node" {
		return fmt.Errorf("mode must be 'client' or 'exit-node', got %q", c.Mode)
	}
	if len(c.Routes) == 0 {
		return fmt.Errorf("no routes defined")
	}
	if c.Mode == "client" {
		if err := validateLocalSOCKSAddress(c.SOCKS5); err != nil {
			return fmt.Errorf("socks5: %w", err)
		}
	}

	enabledRoutes := 0
	for i, r := range c.Routes {
		if r.Disabled {
			continue
		}
		enabledRoutes++
		if r.Transport == "" {
			return fmt.Errorf("route %d: transport is required", i)
		}

		var err error
		switch r.Transport {
		case "wss":
			err = validateWSSRoute(c.Mode, c.SOCKS5, r)
		case "yandex", "vyandex", "cupsonline":
			if r.URL == "" {
				err = fmt.Errorf("%s transport requires 'url'", r.Transport)
			} else if parsed, parseErr := url.ParseRequestURI(r.URL); parseErr != nil || parsed.Scheme == "" || parsed.Host == "" {
				err = fmt.Errorf("%s url must be an absolute http/https URL", r.Transport)
			} else if parsed.Scheme != "http" && parsed.Scheme != "https" {
				err = fmt.Errorf("%s url must use http or https", r.Transport)
			}
		case "oneme":
			if strings.TrimSpace(r.MaxToken) == "" {
				err = fmt.Errorf("oneme transport requires 'maxToken'")
			} else if c.Mode == "client" {
				uid := strings.TrimSpace(r.MaxUID)
				parsedUID, parseErr := strconv.ParseInt(uid, 10, 64)
				if uid == "" {
					err = fmt.Errorf("oneme transport in client mode requires 'maxUid'")
				} else if parseErr != nil || parsedUID <= 0 {
					err = fmt.Errorf("oneme 'maxUid' must be a positive integer")
				}
			}
		case "direct":
			err = fmt.Errorf("direct transport is not implemented")
		default:
			err = fmt.Errorf("unknown transport %q", r.Transport)
		}
		if err != nil {
			return fmt.Errorf("route %d: %w", i, err)
		}
	}

	if enabledRoutes == 0 {
		return fmt.Errorf("no enabled routes defined")
	}
	if c.PingEvery <= 0 {
		return fmt.Errorf("pingEvery must be positive, got %v", c.PingEvery)
	}
	return nil
}

func validateWSSRoute(mode, socksAddr string, r RouteConfig) error {
	if err := validateAuthToken(r.AuthToken); err != nil {
		return err
	}
	if mode == "client" {
		if err := validateTunnelEndpoint(r.Endpoint); err != nil {
			return err
		}
		if r.ProxyURL != "" {
			proxyAddr, err := validateProxyURL(r.ProxyURL)
			if err != nil {
				return err
			}
			if sameLocalEndpoint(proxyAddr, socksAddr) {
				return fmt.Errorf("proxyURL points to the local OpenFlux SOCKS5 listener")
			}
		}
		return nil
	}

	if err := validateTCPAddress(r.ListenAddr, true); err != nil {
		return fmt.Errorf("invalid listenAddr: %w", err)
	}
	if strings.TrimSpace(r.CertFile) == "" {
		return fmt.Errorf("wss exit-node requires 'certFile'")
	}
	if strings.TrimSpace(r.KeyFile) == "" {
		return fmt.Errorf("wss exit-node requires 'keyFile'")
	}
	for _, cidr := range r.DenyCIDRs {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(cidr))
		if err != nil {
			return fmt.Errorf("invalid denyCIDR %q: %w", cidr, err)
		}
		if !prefix.Addr().Is4() {
			return fmt.Errorf("denyCIDR %q is IPv6; this release is IPv4-only", cidr)
		}
	}
	return nil
}

func validateAuthToken(token string) error {
	if token != strings.TrimSpace(token) || strings.IndexFunc(token, unicode.IsSpace) >= 0 {
		return fmt.Errorf("wss authToken must not contain whitespace")
	}
	if len(token) < 32 {
		return fmt.Errorf("wss authToken must contain at least 32 characters")
	}
	if len(token) > 4096 {
		return fmt.Errorf("wss authToken is too long")
	}
	return nil
}

func validateTunnelEndpoint(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil || !u.IsAbs() || u.Host == "" {
		return fmt.Errorf("wss client requires an absolute 'endpoint'")
	}
	if u.Scheme != "wss" {
		return fmt.Errorf("wss endpoint must use wss://")
	}
	if u.User != nil {
		return fmt.Errorf("wss endpoint must not contain user information")
	}
	if u.Fragment != "" || u.RawQuery != "" {
		return fmt.Errorf("wss endpoint must not contain a query or fragment")
	}
	if u.Path != wssTunnelPath {
		return fmt.Errorf("wss endpoint path must be %s", wssTunnelPath)
	}
	if port := u.Port(); port != "" {
		if _, err := parsePort(port); err != nil {
			return fmt.Errorf("wss endpoint: %w", err)
		}
	}
	return nil
}

func validateProxyURL(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil || !u.IsAbs() || u.Hostname() == "" {
		return "", fmt.Errorf("proxyURL must be an absolute URL")
	}
	switch u.Scheme {
	case "http", "socks5":
	default:
		return "", fmt.Errorf("proxyURL scheme must be http or socks5")
	}
	if u.Fragment != "" || u.RawQuery != "" || (u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("proxyURL must not contain a path, query, or fragment")
	}
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "http":
			port = "80"
		default:
			return "", fmt.Errorf("socks5 proxyURL requires an explicit port")
		}
	}
	if _, err := parsePort(port); err != nil {
		return "", fmt.Errorf("proxyURL: %w", err)
	}
	return net.JoinHostPort(u.Hostname(), port), nil
}

func validateLocalSOCKSAddress(address string) error {
	if err := validateTCPAddress(address, false); err != nil {
		return err
	}
	host, _, _ := net.SplitHostPort(address)
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !ip.IsLoopback() {
		return fmt.Errorf("must bind to a loopback address")
	}
	return nil
}

func validateTCPAddress(address string, allowUnspecified bool) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if !allowUnspecified && host == "" {
		return fmt.Errorf("host is required")
	}
	if _, err := parsePort(port); err != nil {
		return err
	}
	return nil
}

func parsePort(port string) (uint16, error) {
	value, err := strconv.ParseUint(port, 10, 16)
	if err != nil || value == 0 {
		return 0, fmt.Errorf("port must be an integer from 1 to 65535")
	}
	return uint16(value), nil
}

func sameLocalEndpoint(a, b string) bool {
	hostA, portA, errA := net.SplitHostPort(a)
	hostB, portB, errB := net.SplitHostPort(b)
	if errA != nil || errB != nil || portA != portB {
		return false
	}
	return normalizedLocalHost(hostA) == normalizedLocalHost(hostB)
}

func normalizedLocalHost(host string) string {
	if strings.EqualFold(host, "localhost") {
		return "loopback"
	}
	if ip, err := netip.ParseAddr(host); err == nil && ip.IsLoopback() {
		return "loopback"
	}
	return strings.ToLower(strings.TrimSuffix(host, "."))
}

// configPaths returns standard config file locations in priority order.
func configPaths() []string {
	home, _ := os.UserHomeDir()

	switch runtime.GOOS {
	case "windows":
		appData := os.Getenv("APPDATA")
		if appData == "" {
			appData = filepath.Join(home, "AppData", "Roaming")
		}
		return []string{
			filepath.Join(appData, "OpenFlux", "flux.json"),
			filepath.Join(home, ".openflux", "flux.json"),
			"flux.json",
		}
	case "darwin":
		return []string{
			filepath.Join(home, "Library", "Application Support", "OpenFlux", "flux.json"),
			filepath.Join(home, ".openflux", "flux.json"),
			"flux.json",
		}
	default:
		xdgConfig := os.Getenv("XDG_CONFIG_HOME")
		if xdgConfig == "" {
			xdgConfig = filepath.Join(home, ".config")
		}
		return []string{
			filepath.Join(xdgConfig, "openflux", "flux.json"),
			filepath.Join(home, ".openflux", "flux.json"),
			"flux.json",
		}
	}
}

// FindConfig searches standard locations and returns the first existing path.
func FindConfig() string {
	for _, path := range configPaths() {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return ""
}

// LoadConfigAuto finds and loads config from standard locations.
func LoadConfigAuto() (Config, error) {
	path := FindConfig()
	if path == "" {
		return Config{}, fmt.Errorf("no config file found in standard locations")
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		return cfg, fmt.Errorf("loading %s: %w", path, err)
	}
	return cfg, nil
}

// GenerateDefault creates a client WSS configuration at the primary location.
func GenerateDefault() (string, error) {
	cfg := DefaultConfig()
	cfg.Routes = []RouteConfig{
		{
			Transport:  "wss",
			Endpoint:   "wss://127.0.0.1:8443/openflux/v1/tunnel",
			AuthToken:  "${OPENFLUX_AUTH_TOKEN}",
			CAFile:     "openflux-ca.pem",
			ServerName: "openflux.local",
			Exit:       "controlled-vps",
		},
		{
			Transport: "yandex",
			URL:       "https://docs.yandex.ru/docs/YOUR_DOC_ID",
			Disabled:  true,
		},
		{
			Transport: "oneme",
			MaxToken:  "${MAX_TOKEN}",
			MaxUID:    "${MAX_UID}",
			Disabled:  true,
		},
	}

	path := configPaths()[0]
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("creating config directory: %w", err)
	}
	if err := cfg.Save(path); err != nil {
		return "", fmt.Errorf("saving config: %w", err)
	}
	return path, nil
}
