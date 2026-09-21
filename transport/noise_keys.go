package transport

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/flynn/noise"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/scrypt"
)

// noiseSuite is the cipher suite of the v2 encrypted transport:
// X25519 key agreement, AES-256-GCM, SHA-256.
var noiseSuite = noise.NewCipherSuite(noise.DH25519, noise.CipherAESGCM, noise.HashSHA256)

const (
	staticKeySize = 32
	minSecretLen  = 16
)

// GenerateStaticKey creates a fresh X25519 static key pair.
func GenerateStaticKey() (noise.DHKey, error) {
	return noiseSuite.GenerateKeypair(rand.Reader)
}

// LoadOrCreateStaticKey reads the exit node's static key from path, creating
// a new one (file mode 0600) when the file does not exist yet. The file holds
// the base64 private key; the public key is recomputed on load. created
// reports whether a new key was generated.
func LoadOrCreateStaticKey(path string) (key noise.DHKey, created bool, err error) {
	raw, err := os.ReadFile(path)
	if err == nil {
		priv, decErr := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
		if decErr != nil || len(priv) != staticKeySize {
			return noise.DHKey{}, false, fmt.Errorf("static key file %s: not a base64 %d-byte key", path, staticKeySize)
		}
		pub, pubErr := curve25519.X25519(priv, curve25519.Basepoint)
		if pubErr != nil {
			return noise.DHKey{}, false, fmt.Errorf("static key file %s: %w", path, pubErr)
		}
		return noise.DHKey{Private: priv, Public: pub}, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return noise.DHKey{}, false, fmt.Errorf("read static key file: %w", err)
	}
	key, err = GenerateStaticKey()
	if err != nil {
		return noise.DHKey{}, false, fmt.Errorf("generate static key: %w", err)
	}
	encoded := base64.StdEncoding.EncodeToString(key.Private) + "\n"
	if err := os.WriteFile(path, []byte(encoded), 0o600); err != nil {
		return noise.DHKey{}, false, fmt.Errorf("write static key file: %w", err)
	}
	return key, true, nil
}

// PublicKeyString renders a public key the way operators pass it around.
func PublicKeyString(pub []byte) string {
	return base64.StdEncoding.EncodeToString(pub)
}

// ParsePublicKey parses a base64 X25519 public key, ignoring surrounding
// whitespace.
func ParsePublicKey(s string) ([]byte, error) {
	pub, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("public key is not base64: %w", err)
	}
	if len(pub) != staticKeySize {
		return nil, fmt.Errorf("public key is %d bytes, want %d", len(pub), staticKeySize)
	}
	return pub, nil
}

// pskSalt is a fixed, public domain-separation salt: the PSK is a
// password-derived key, its secrecy comes from the secret alone.
var pskSalt = sha256.Sum256([]byte("OpenFlux encrypted transport v2 psk"))

// DerivePSK stretches an operator-chosen secret into the 32-byte Noise
// pre-shared key with scrypt, so that a captured handshake cannot be used
// to test guesses cheaply.
func DerivePSK(secret string) ([]byte, error) {
	if len(secret) < minSecretLen {
		return nil, fmt.Errorf("encryption secret must contain at least %d characters", minSecretLen)
	}
	return scrypt.Key([]byte(secret), pskSalt[:], 32768, 8, 1, 32)
}
