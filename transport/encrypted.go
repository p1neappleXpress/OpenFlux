package transport

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"unicode/utf8"

	"golang.org/x/crypto/scrypt"
)

const (
	encryptedVersion = byte(1)
	encryptedHeader  = 5
	maxSeenNonces    = 4096
)

var encryptedMagic = [3]byte{'O', 'F', 'X'}

// EncryptedTransport wraps another Transport with end-to-end AES-256-GCM
// authenticated encryption, so the transport's own provider only ever
// observes ciphertext. Keys are derived from a shared secret via scrypt; the
// context string is just a public, per-session KDF salt - secrecy comes
// exclusively from the secret, which both peers must share out of band.
//
// Each direction (client->exit, exit->client) uses its own derived key, so a
// compromise of one direction's traffic does not help decrypt the other.
// Every packet also carries a random nonce and is checked against a bounded
// replay window, so a captured packet cannot be replayed back at either
// peer.
type EncryptedTransport struct {
	Transport
	sendAEAD      cipher.AEAD
	receiveAEAD   cipher.AEAD
	sendDirection byte
	recvDirection byte
	seenMu        sync.Mutex
	seen          map[string]struct{}
	seenOrder     []string
}

// NewEncryptedTransport wraps inner with a directional AES-256-GCM stream.
// Both peers must be configured with the same secret and context, and
// exactly one of them must set exitNode=true so the two sides pick opposite
// send/receive key pairs.
func NewEncryptedTransport(inner Transport, secret, context string, exitNode bool) (*EncryptedTransport, error) {
	if inner == nil {
		return nil, errors.New("inner transport is nil")
	}
	master, err := DeriveEncryptionKey(secret, context)
	if err != nil {
		return nil, err
	}
	defer clear(master)
	return NewEncryptedTransportWithKey(inner, master, exitNode)
}

// DeriveEncryptionKey uses the existing v1 scrypt KDF unchanged. The returned
// 32-byte key is as sensitive as the secret. Mobile apps may prepare and store
// it in shared Keychain outside their memory-constrained Network Extension.
func DeriveEncryptionKey(secret, context string) ([]byte, error) {
	if !utf8.ValidString(secret) || utf8.RuneCountInString(secret) < 16 {
		return nil, errors.New("encryption secret must contain at least 16 characters")
	}

	salt := sha256.Sum256([]byte("OpenFlux encrypted transport v1\x00" + context))
	master, err := scrypt.Key([]byte(secret), salt[:], 32768, 8, 1, 32)
	if err != nil {
		return nil, fmt.Errorf("derive encryption key: %w", err)
	}
	return master, nil
}

// NewEncryptedTransportWithKey consumes a previously derived v1 master key.
// It shares all direction, nonce, replay and framing logic with the secret API.
func NewEncryptedTransportWithKey(inner Transport, master []byte, exitNode bool) (*EncryptedTransport, error) {
	if inner == nil || len(master) != 32 {
		return nil, errors.New("invalid encrypted transport configuration")
	}
	clientToExit := deriveDirectionalKey(master, "client-to-exit")
	exitToClient := deriveDirectionalKey(master, "exit-to-client")

	sendKey, receiveKey := clientToExit, exitToClient
	sendDirection, receiveDirection := byte(0), byte(1)
	if exitNode {
		sendKey, receiveKey = exitToClient, clientToExit
		sendDirection, receiveDirection = 1, 0
	}
	sendAEAD, err := newGCM(sendKey)
	if err != nil {
		return nil, err
	}
	receiveAEAD, err := newGCM(receiveKey)
	if err != nil {
		return nil, err
	}
	return &EncryptedTransport{
		Transport:     inner,
		sendAEAD:      sendAEAD,
		receiveAEAD:   receiveAEAD,
		sendDirection: sendDirection,
		recvDirection: receiveDirection,
		seen:          make(map[string]struct{}),
	}, nil
}

func deriveDirectionalKey(master []byte, label string) []byte {
	mac := hmac.New(sha256.New, master)
	_, _ = mac.Write([]byte("OpenFlux direction v1\x00" + label))
	return mac.Sum(nil)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create AES cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create AES-GCM: %w", err)
	}
	return aead, nil
}

func (e *EncryptedTransport) Send(data []byte) error {
	header := []byte{encryptedMagic[0], encryptedMagic[1], encryptedMagic[2], encryptedVersion, e.sendDirection}
	nonce := make([]byte, e.sendAEAD.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("create packet nonce: %w", err)
	}
	packet := make([]byte, 0, len(header)+len(nonce)+len(data)+e.sendAEAD.Overhead())
	packet = append(packet, header...)
	packet = append(packet, nonce...)
	packet = e.sendAEAD.Seal(packet, nonce, data, header)
	return e.Transport.Send(packet)
}

func (e *EncryptedTransport) Receive(callback func([]byte)) {
	e.Transport.Receive(func(packet []byte) {
		if len(packet) < encryptedHeader+e.receiveAEAD.NonceSize()+e.receiveAEAD.Overhead() {
			return
		}
		header := packet[:encryptedHeader]
		if header[0] != encryptedMagic[0] || header[1] != encryptedMagic[1] ||
			header[2] != encryptedMagic[2] || header[3] != encryptedVersion ||
			header[4] != e.recvDirection {
			return
		}
		nonceEnd := encryptedHeader + e.receiveAEAD.NonceSize()
		nonce := packet[encryptedHeader:nonceEnd]
		plaintext, err := e.receiveAEAD.Open(nil, nonce, packet[nonceEnd:], header)
		if err != nil || !e.rememberNonce(nonce) {
			return
		}
		callback(plaintext)
	})
}

func (e *EncryptedTransport) rememberNonce(nonce []byte) bool {
	key := string(nonce)
	e.seenMu.Lock()
	defer e.seenMu.Unlock()
	if _, exists := e.seen[key]; exists {
		return false
	}
	e.seen[key] = struct{}{}
	e.seenOrder = append(e.seenOrder, key)
	if len(e.seenOrder) > maxSeenNonces {
		oldest := e.seenOrder[0]
		e.seenOrder = e.seenOrder[1:]
		delete(e.seen, oldest)
	}
	return true
}
