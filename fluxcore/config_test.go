package fluxcore

import (
	"os"
	"strings"
	"testing"
)

const testAuthToken = "0123456789abcdef0123456789abcdef"

func configWithRoute(mode string, route RouteConfig) Config {
	cfg := DefaultConfig()
	cfg.Mode = mode
	cfg.Routes = []RouteConfig{route}
	return cfg
}

func validWSSClientRoute() RouteConfig {
	return RouteConfig{
		Transport: "wss",
		Endpoint:  "wss://exit.example.test/openflux/v1/tunnel",
		AuthToken: testAuthToken,
	}
}

func validWSSExitRoute() RouteConfig {
	return RouteConfig{
		Transport:  "wss",
		AuthToken:  testAuthToken,
		ListenAddr: "127.0.0.1:8443",
		CertFile:   "server.crt",
		KeyFile:    "server.key",
	}
}

func TestConfigValidateWSSByMode(t *testing.T) {
	tests := []struct {
		name    string
		mode    string
		route   RouteConfig
		wantErr string
	}{
		{name: "client valid", mode: "client", route: validWSSClientRoute()},
		{name: "exit valid", mode: "exit-node", route: validWSSExitRoute()},
		{name: "weak token", mode: "client", route: RouteConfig{Transport: "wss", Endpoint: "wss://exit.example.test/openflux/v1/tunnel", AuthToken: "short"}, wantErr: "at least 32"},
		{name: "token whitespace", mode: "client", route: RouteConfig{Transport: "wss", Endpoint: "wss://exit.example.test/openflux/v1/tunnel", AuthToken: testAuthToken + " "}, wantErr: "whitespace"},
		{name: "missing endpoint", mode: "client", route: RouteConfig{Transport: "wss", AuthToken: testAuthToken}, wantErr: "absolute 'endpoint'"},
		{name: "plaintext websocket", mode: "client", route: RouteConfig{Transport: "wss", Endpoint: "ws://exit.example.test/openflux/v1/tunnel", AuthToken: testAuthToken}, wantErr: "wss://"},
		{name: "endpoint userinfo", mode: "client", route: RouteConfig{Transport: "wss", Endpoint: "wss://user:pass@exit.example.test/openflux/v1/tunnel", AuthToken: testAuthToken}, wantErr: "user information"},
		{name: "endpoint query", mode: "client", route: RouteConfig{Transport: "wss", Endpoint: "wss://exit.example.test/openflux/v1/tunnel?q=x", AuthToken: testAuthToken}, wantErr: "query or fragment"},
		{name: "wrong path", mode: "client", route: RouteConfig{Transport: "wss", Endpoint: "wss://exit.example.test/other", AuthToken: testAuthToken}, wantErr: "endpoint path"},
		{name: "missing listen", mode: "exit-node", route: RouteConfig{Transport: "wss", AuthToken: testAuthToken, CertFile: "server.crt", KeyFile: "server.key"}, wantErr: "listenAddr"},
		{name: "missing cert", mode: "exit-node", route: RouteConfig{Transport: "wss", AuthToken: testAuthToken, ListenAddr: "127.0.0.1:8443", KeyFile: "server.key"}, wantErr: "certFile"},
		{name: "missing key", mode: "exit-node", route: RouteConfig{Transport: "wss", AuthToken: testAuthToken, ListenAddr: "127.0.0.1:8443", CertFile: "server.crt"}, wantErr: "keyFile"},
		{name: "bad deny CIDR", mode: "exit-node", route: RouteConfig{Transport: "wss", AuthToken: testAuthToken, ListenAddr: "127.0.0.1:8443", CertFile: "server.crt", KeyFile: "server.key", DenyCIDRs: []string{"not-a-cidr"}}, wantErr: "invalid denyCIDR"},
		{name: "IPv6 deny CIDR", mode: "exit-node", route: RouteConfig{Transport: "wss", AuthToken: testAuthToken, ListenAddr: "127.0.0.1:8443", CertFile: "server.crt", KeyFile: "server.key", DenyCIDRs: []string{"2001:db8::/32"}}, wantErr: "IPv4-only"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := configWithRoute(tt.mode, tt.route).Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestConfigValidateWSSProxy(t *testing.T) {
	tests := []struct {
		name    string
		proxy   string
		socks   string
		wantErr string
	}{
		{name: "socks bootstrap", proxy: "socks5://127.0.0.1:10808", socks: "127.0.0.1:1080"},
		{name: "HTTP proxy", proxy: "http://proxy.example.test:3128", socks: "127.0.0.1:1080"},
		{name: "HTTPS proxy unsupported", proxy: "https://proxy.example.test:443", socks: "127.0.0.1:1080", wantErr: "scheme"},
		{name: "unsupported scheme", proxy: "ftp://proxy.example.test:21", socks: "127.0.0.1:1080", wantErr: "scheme"},
		{name: "SOCKS missing port", proxy: "socks5://proxy.example.test", socks: "127.0.0.1:1080", wantErr: "explicit port"},
		{name: "proxy path", proxy: "http://proxy.example.test:3128/path", socks: "127.0.0.1:1080", wantErr: "path"},
		{name: "exact loop", proxy: "socks5://127.0.0.1:1080", socks: "127.0.0.1:1080", wantErr: "local OpenFlux SOCKS5"},
		{name: "localhost loop", proxy: "socks5://localhost:1080", socks: "127.0.0.1:1080", wantErr: "local OpenFlux SOCKS5"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			route := validWSSClientRoute()
			route.ProxyURL = tt.proxy
			cfg := configWithRoute("client", route)
			cfg.SOCKS5 = tt.socks
			err := cfg.Validate()
			if tt.wantErr == "" && err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("Validate() error = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestConfigValidateSOCKS5MustBeLocal(t *testing.T) {
	tests := []struct {
		address string
		wantErr string
	}{
		{address: ":1080", wantErr: "host is required"},
		{address: "0.0.0.0:1080", wantErr: "loopback"},
		{address: "192.0.2.10:1080", wantErr: "loopback"},
	}
	for _, tt := range tests {
		t.Run(tt.address, func(t *testing.T) {
			cfg := configWithRoute("client", validWSSClientRoute())
			cfg.SOCKS5 = tt.address
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestConfigExpandEnvVarsWSS(t *testing.T) {
	t.Setenv("OF_ENDPOINT", "wss://exit.example.test/openflux/v1/tunnel")
	t.Setenv("OF_TOKEN", testAuthToken)
	t.Setenv("OF_CA", "ca.pem")
	t.Setenv("OF_NAME", "exit.example.test")
	t.Setenv("OF_PROXY", "socks5://127.0.0.1:10808")
	t.Setenv("OF_LISTEN", "127.0.0.1:8443")
	t.Setenv("OF_CERT", "server.crt")
	t.Setenv("OF_KEY", "server.key")
	t.Setenv("OF_DENY", "203.0.113.10/32")

	cfg := configWithRoute("client", RouteConfig{
		Transport:  "wss",
		Endpoint:   "${OF_ENDPOINT}",
		AuthToken:  "${OF_TOKEN}",
		CAFile:     "${OF_CA}",
		ServerName: "${OF_NAME}",
		ProxyURL:   "${OF_PROXY}",
		ListenAddr: "${OF_LISTEN}",
		CertFile:   "${OF_CERT}",
		KeyFile:    "${OF_KEY}",
		DenyCIDRs:  []string{"${OF_DENY}"},
	})
	cfg.ExpandEnvVars()
	r := cfg.Routes[0]
	if r.Endpoint != os.Getenv("OF_ENDPOINT") || r.AuthToken != testAuthToken ||
		r.CAFile != "ca.pem" || r.ServerName != "exit.example.test" ||
		r.ProxyURL != "socks5://127.0.0.1:10808" || r.ListenAddr != "127.0.0.1:8443" ||
		r.CertFile != "server.crt" || r.KeyFile != "server.key" || r.DenyCIDRs[0] != "203.0.113.10/32" {
		t.Fatalf("ExpandEnvVars() did not expand all WSS fields: %+v", r)
	}
}

func TestConfigValidateOneMeByMode(t *testing.T) {
	tests := []struct {
		name    string
		mode    string
		route   RouteConfig
		wantErr string
	}{
		{name: "exit node only needs token", mode: "exit-node", route: RouteConfig{Transport: "oneme", MaxToken: "token"}},
		{name: "exit node needs token", mode: "exit-node", route: RouteConfig{Transport: "oneme"}, wantErr: "requires 'maxToken'"},
		{name: "client needs token", mode: "client", route: RouteConfig{Transport: "oneme", MaxUID: "123"}, wantErr: "requires 'maxToken'"},
		{name: "client needs uid", mode: "client", route: RouteConfig{Transport: "oneme", MaxToken: "token"}, wantErr: "requires 'maxUid'"},
		{name: "client uid must be numeric", mode: "client", route: RouteConfig{Transport: "oneme", MaxToken: "token", MaxUID: "not-a-number"}, wantErr: "positive integer"},
		{name: "client uid must not be zero", mode: "client", route: RouteConfig{Transport: "oneme", MaxToken: "token", MaxUID: "0"}, wantErr: "positive integer"},
		{name: "client uid must not be negative", mode: "client", route: RouteConfig{Transport: "oneme", MaxToken: "token", MaxUID: "-1"}, wantErr: "positive integer"},
		{name: "client accepts positive uid", mode: "client", route: RouteConfig{Transport: "oneme", MaxToken: "token", MaxUID: "123"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := configWithRoute(tt.mode, tt.route).Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestConfigValidateSkipsDisabledRoutes(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Routes = []RouteConfig{
		{Transport: "yandex", URL: "https://example.test/document"},
		{Transport: "wss", Disabled: true},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestConfigValidateRequiresEnabledRoute(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Routes = []RouteConfig{{Transport: "wss", Disabled: true}}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "no enabled routes") {
		t.Fatalf("Validate() error = %v, want no enabled routes error", err)
	}
}

func TestConfigValidateRejectsUnimplementedDirectTransport(t *testing.T) {
	cfg := configWithRoute("client", RouteConfig{Transport: "direct", DirectAddr: "exit.example.test:443"})
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("Validate() error = %v, want not implemented error", err)
	}
}

func TestDefaultConfigBindsSOCKS5Locally(t *testing.T) {
	if got := DefaultConfig().SOCKS5; got != "127.0.0.1:1080" {
		t.Fatalf("DefaultConfig().SOCKS5 = %q, want local-only bind", got)
	}
}

func TestLoadConfigFillsLocalSOCKS5Default(t *testing.T) {
	path := t.TempDir() + "/config.json"
	data := []byte(`{
		"mode": "client",
		"socks5": "",
		"routes": [{"transport": "yandex", "url": "https://example.test/document"}]
	}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SOCKS5 != "127.0.0.1:1080" {
		t.Fatalf("LoadConfig().SOCKS5 = %q, want local-only bind", cfg.SOCKS5)
	}
}
