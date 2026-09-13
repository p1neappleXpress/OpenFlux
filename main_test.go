package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	fluxcore "fluxcore"

	"universal-bypass-tool/socks5"
)

func TestFirstEnabledRoute(t *testing.T) {
	routes := []fluxcore.RouteConfig{
		{Transport: "yandex", Disabled: true},
		{Transport: "wss", Endpoint: "wss://example.com/openflux/v1/tunnel"},
		{Transport: "oneme"},
	}
	route, index, ok := firstEnabledRoute(routes)
	if !ok || index != 1 || route.Transport != "wss" {
		t.Fatalf("firstEnabledRoute() = (%#v, %d, %v)", route, index, ok)
	}

	if _, index, ok := firstEnabledRoute([]fluxcore.RouteConfig{{Disabled: true}}); ok || index != -1 {
		t.Fatalf("disabled-only routes returned index=%d ok=%v", index, ok)
	}
}

func TestResolveWSSFilePaths(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config", "flux.json")
	absoluteCA := filepath.Join(t.TempDir(), "ca.pem")
	route := resolveWSSFilePaths(configPath, fluxcore.RouteConfig{
		Transport: "wss",
		CAFile:    absoluteCA,
		CertFile:  "exit-cert.pem",
		KeyFile:   filepath.Join("tls", "exit-key.pem"),
	})

	if route.CAFile != absoluteCA {
		t.Fatalf("CAFile = %q, want absolute path unchanged", route.CAFile)
	}
	if want := filepath.Join(filepath.Dir(configPath), "exit-cert.pem"); route.CertFile != want {
		t.Fatalf("CertFile = %q, want %q", route.CertFile, want)
	}
	if want := filepath.Join(filepath.Dir(configPath), "tls", "exit-key.pem"); route.KeyFile != want {
		t.Fatalf("KeyFile = %q, want %q", route.KeyFile, want)
	}
}

func TestResolveWSSFilePathsLeavesLegacyRouteUnchanged(t *testing.T) {
	route := fluxcore.RouteConfig{Transport: "yandex", CAFile: "relative.pem"}
	if got := resolveWSSFilePaths("config/flux.json", route); got.CAFile != route.CAFile {
		t.Fatalf("legacy route CAFile = %q, want %q", got.CAFile, route.CAFile)
	}
}

func TestExampleConfigsValidate(t *testing.T) {
	t.Setenv("OPENFLUX_AUTH_TOKEN", "0123456789abcdef0123456789abcdef")
	tests := []struct {
		path string
		mode string
	}{
		{path: "flux.example.json", mode: "client"},
		{path: "flux.exit.example.json", mode: "exit-node"},
	}

	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			config, err := fluxcore.LoadConfig(test.path)
			if err != nil {
				t.Fatal(err)
			}
			config.ExpandEnvVars()
			if err := config.Validate(); err != nil {
				t.Fatalf("validate example: %v", err)
			}
			if config.Mode != test.mode {
				t.Fatalf("mode = %q, want %q", config.Mode, test.mode)
			}
			if config.Routes[0].AuthToken != os.Getenv("OPENFLUX_AUTH_TOKEN") {
				t.Fatal("example token environment reference was not expanded")
			}
		})
	}
}

func TestServeSOCKSUntilCanceled(t *testing.T) {
	server := socks5.NewSOCKS5Server("127.0.0.1:0", nil)
	if err := server.Bind(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serveSOCKSUntilCanceled(ctx, server)
	}()
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serveSOCKSUntilCanceled() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SOCKS5 server did not stop after cancellation")
	}
}
