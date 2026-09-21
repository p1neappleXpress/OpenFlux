package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"openflux/transport"
)

// nopTransport is the bare minimum to wrap.
type nopTransport struct{}

func (nopTransport) Start() error                    { return nil }
func (nopTransport) Stop() error                     { return nil }
func (nopTransport) Send([]byte) error               { return nil }
func (nopTransport) Receive(func([]byte))            {}
func (nopTransport) IsConnected() bool               { return false }
func (nopTransport) Stats() transport.TransportStats { return transport.TransportStats{} }

func TestEncryptionSetupOffWithoutFlags(t *testing.T) {
	for _, initiator := range []bool{true, false} {
		setup, err := newEncryptionSetup(encryptionOptions{}, initiator)
		if err != nil {
			t.Fatal(err)
		}
		if setup != nil {
			t.Fatal("encryption configured without any flag")
		}
	}
}

func TestEncryptionSetupRejectsMisplacedFlags(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "exit.key")
	cases := []struct {
		name      string
		opts      encryptionOptions
		initiator bool
		wantErr   string
	}{
		{"exit key on the client", encryptionOptions{ExitKeyFile: keyFile}, true, "--exit-key-file"},
		{"peer key on the exit", encryptionOptions{PeerKey: "AAAA"}, false, "--peer-key"},
		{"psk alone on the client", encryptionOptions{PSK: "a sufficiently long shared secret"}, true, "--peer-key"},
		{"psk alone on the exit", encryptionOptions{PSK: "a sufficiently long shared secret"}, false, "--exit-key-file"},
		{"bad peer key", encryptionOptions{PeerKey: "not a key"}, true, "peer key"},
		{"short psk", encryptionOptions{PeerKey: validPeerKey(t), PSK: "short"}, true, "16"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newEncryptionSetup(tc.opts, tc.initiator)
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

func validPeerKey(t *testing.T) string {
	t.Helper()
	key, err := transport.GenerateStaticKey()
	if err != nil {
		t.Fatal(err)
	}
	return transport.PublicKeyString(key.Public)
}

func TestEncryptionSetupExitCreatesKeyAndAnnouncesIt(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "exit.key")
	setup, err := newEncryptionSetup(encryptionOptions{ExitKeyFile: keyFile}, false)
	if err != nil {
		t.Fatal(err)
	}
	key, created, err := transport.LoadOrCreateStaticKey(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("setup did not create the key file")
	}
	if !strings.Contains(setup.banner, transport.PublicKeyString(key.Public)) {
		t.Fatalf("banner %q lacks the public key", setup.banner)
	}
	if !strings.Contains(setup.banner, "--peer-key") {
		t.Fatalf("banner %q does not tell the operator where the key goes", setup.banner)
	}
	wrapped, err := setup.wrap(nopTransport{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := wrapped.(*transport.EncryptedTransport); !ok {
		t.Fatalf("wrap returned %T", wrapped)
	}
}

func TestEncryptionSetupClientUsesPeerKey(t *testing.T) {
	setup, err := newEncryptionSetup(encryptionOptions{PeerKey: validPeerKey(t)}, true)
	if err != nil {
		t.Fatal(err)
	}
	if setup.banner != "" {
		t.Fatalf("client has a banner: %q", setup.banner)
	}
	wrapped, err := setup.wrap(nopTransport{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := wrapped.(*transport.EncryptedTransport); !ok {
		t.Fatalf("wrap returned %T", wrapped)
	}
}

func TestEncryptionSetupLabelsClosedNode(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "exit.key")
	open, err := newEncryptionSetup(encryptionOptions{ExitKeyFile: keyFile}, false)
	if err != nil {
		t.Fatal(err)
	}
	closed, err := newEncryptionSetup(encryptionOptions{ExitKeyFile: keyFile, PSK: "a sufficiently long shared secret"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if open.label == closed.label {
		t.Fatalf("open and closed node share the label %q", open.label)
	}
	if !strings.Contains(closed.label, "PSK") {
		t.Fatalf("closed node label %q does not mention the PSK", closed.label)
	}
}

func TestReadSecretFileTrimsWhitespace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "psk.txt")
	if err := os.WriteFile(path, []byte("  a sufficiently long shared secret \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	secret, err := readSecretFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if secret != "a sufficiently long shared secret" {
		t.Fatalf("got %q", secret)
	}
	if _, err := readSecretFile(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing file accepted")
	}
}
