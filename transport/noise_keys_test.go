package transport

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLoadOrCreateStaticKeyCreatesThenReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exit.key")

	first, created, err := LoadOrCreateStaticKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("first call did not report creation")
	}
	if len(first.Public) != 32 || len(first.Private) != 32 {
		t.Fatalf("key sizes %d/%d, want 32/32", len(first.Public), len(first.Private))
	}

	second, created, err := LoadOrCreateStaticKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("second call reported creation")
	}
	if !bytes.Equal(first.Public, second.Public) || !bytes.Equal(first.Private, second.Private) {
		t.Fatal("reloaded key differs from the created one")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(raw)))
	if err != nil || !bytes.Equal(decoded, first.Private) {
		t.Fatalf("file is not the base64 private key: %q (%v)", raw, err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("key file mode %o, want 600", perm)
		}
	}
}

func TestLoadOrCreateStaticKeyRejectsCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exit.key")
	if err := os.WriteFile(path, []byte("not a key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadOrCreateStaticKey(path); err == nil {
		t.Fatal("corrupt key file was accepted")
	}
}

func TestPublicKeyStringRoundTrip(t *testing.T) {
	key, err := GenerateStaticKey()
	if err != nil {
		t.Fatal(err)
	}
	s := PublicKeyString(key.Public)
	pub, err := ParsePublicKey(s)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pub, key.Public) {
		t.Fatal("public key did not survive the string round trip")
	}
	if _, err := ParsePublicKey(" " + s + "\n"); err != nil {
		t.Fatalf("surrounding whitespace rejected: %v", err)
	}
}

func TestParsePublicKeyRejectsBadInput(t *testing.T) {
	for _, in := range []string{
		"",
		"not base64 at all!",
		base64.StdEncoding.EncodeToString(make([]byte, 31)),
		base64.StdEncoding.EncodeToString(make([]byte, 33)),
	} {
		if _, err := ParsePublicKey(in); err == nil {
			t.Fatalf("%q accepted", in)
		}
	}
}

func TestDerivePSK(t *testing.T) {
	a, err := DerivePSK("a sufficiently long shared secret")
	if err != nil {
		t.Fatal(err)
	}
	b, err := DerivePSK("a sufficiently long shared secret")
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 32 || !bytes.Equal(a, b) {
		t.Fatalf("derivation not deterministic or wrong size: %d", len(a))
	}
	c, err := DerivePSK("another sufficiently long secret")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, c) {
		t.Fatal("different secrets derived the same PSK")
	}
	if _, err := DerivePSK("too short"); err == nil {
		t.Fatal("short secret accepted")
	}
}
