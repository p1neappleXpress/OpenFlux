package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseConfBasic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.conf")
	body := `# OpenFlux config

[Interface]
Role    = client
Inbound = socks5
Codec   = batched
URL     = https://disk.yandex.ru/i/XXX

[Transport "yandex"]
Type     = yandex
Priority = 50
URL      = https://disk.yandex.ru/i/XXX

[Transport "direct"]
Type     = direct
Priority = 100
Dial     = 1.2.3.4:8443
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	conf, err := parseConf(path)
	if err != nil {
		t.Fatal(err)
	}
	if conf.Interface["Role"] != "client" || conf.Interface["Inbound"] != "socks5" {
		t.Fatalf("Interface: %+v", conf.Interface)
	}
	if len(conf.Transports) != 2 {
		t.Fatalf("transports: %d", len(conf.Transports))
	}
	if conf.Transports[0].Name != "yandex" || conf.Transports[0].Values["Priority"] != "50" {
		t.Fatalf("transport[0]: %+v", conf.Transports[0])
	}
	if conf.Transports[1].Name != "direct" || conf.Transports[1].Values["Dial"] != "1.2.3.4:8443" {
		t.Fatalf("transport[1]: %+v", conf.Transports[1])
	}
}

func TestParseConfInlineComments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.conf")
	body := `[Interface]
Role = client   # this is a comment
URL = https://x   ; another comment
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	conf, err := parseConf(path)
	if err != nil {
		t.Fatal(err)
	}
	if conf.Interface["Role"] != "client" {
		t.Fatalf("Role = %q", conf.Interface["Role"])
	}
	if conf.Interface["URL"] != "https://x" {
		t.Fatalf("URL = %q", conf.Interface["URL"])
	}
}

func TestParseConfBadSection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.conf")
	body := `[Unknown]
Key = value
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := parseConf(path); err == nil {
		t.Fatal("expected error for unknown section")
	}
}

func TestParseConfMissingFile(t *testing.T) {
	if _, err := parseConf(filepath.Join(t.TempDir(), "nope.conf")); err == nil {
		t.Fatal("expected error for missing file")
	}
}
