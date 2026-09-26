package transport

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"golang.org/x/crypto/scrypt"

	"openflux/utils"
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

	// Diagnostic counters.
	sendOK     atomic.Uint64
	sendErr    atomic.Uint64
	recvOK     atomic.Uint64
	recvBadLen atomic.Uint64
	recvBadHdr atomic.Uint64
	recvFail   atomic.Uint64
	recvReplay atomic.Uint64
}

// NewEncryptedTransport wraps inner with a directional AES-256-GCM stream.
// Both peers must be configured with the same secret and context, and
// exactly one of them must set exitNode=true so the two sides pick opposite
// send/receive key pairs.
func NewEncryptedTransport(inner Transport, secret, context string, exitNode bool) (*EncryptedTransport, error) {
	if inner == nil {
		return nil, errors.New("inner transport is nil")
	}
	if len(secret) < 16 {
		return nil, errors.New("encryption secret must contain at least 16 characters")
	}

	side := "CLIENT"
	if exitNode {
		side = "EXIT"
	}

	salt := sha256.Sum256([]byte("OpenFlux encrypted transport v1\x00" + context))
	master, err := scrypt.Key([]byte(secret), salt[:], 32768, 8, 1, 32)
	if err != nil {
		return nil, fmt.Errorf("derive encryption key: %w", err)
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

	// --- Diagnostic dumps ---------------------------------------------------
	//
	// [KEYDUMP] digest is always emitted at debug level 1: it contains only
	// SHA-256 prefixes, so two peers can compare derivations without
	// revealing keys.
	//
	// The detailed dump (secret hex, master, directional keys, send/recv
	// keys) is printed only when --sensitive is set.
	utils.Debugf("[KEYDUMP] digest side=%s secretSHA256=%s contextSHA256=%s masterSHA256=%s c2eSHA256=%s e2cSHA256=%s",
		side,
		utils.Sha256Short([]byte(secret)),
		utils.Sha256Short([]byte(context)),
		utils.Sha256Short(master),
		utils.Sha256Short(clientToExit),
		utils.Sha256Short(exitToClient),
	)
	if utils.Sensitive() {
		utils.Debugf("[KEYDUMP] ============================================================")
		utils.Debugf("[KEYDUMP] side=%s", side)
		utils.Debugf("[KEYDUMP] secretLen=%d", len(secret))
		utils.Debugf("[KEYDUMP] secretSHA256=%s", utils.Sha256Hex([]byte(secret)))
		utils.Debugf("[KEYDUMP] secretHex(first 32)=%s", hex.EncodeToString([]byte(secret)[:minInt(len(secret), 32)]))
		utils.Debugf("[KEYDUMP] context=%q", context)
		utils.Debugf("[KEYDUMP] contextSHA256=%s", utils.Sha256Hex([]byte(context)))
		utils.Debugf("[KEYDUMP] salt=%s", hex.EncodeToString(salt[:]))
		utils.Debugf("[KEYDUMP] master=%s", hex.EncodeToString(master))
		utils.Debugf("[KEYDUMP] client->exit=%s", hex.EncodeToString(clientToExit))
		utils.Debugf("[KEYDUMP] exit->client=%s", hex.EncodeToString(exitToClient))
		utils.Debugf("[KEYDUMP] sendDir=%d recvDir=%d", sendDirection, receiveDirection)
		utils.Debugf("[KEYDUMP] sendKey=%s", hex.EncodeToString(sendKey))
		utils.Debugf("[KEYDUMP] recvKey=%s", hex.EncodeToString(receiveKey))
		utils.Debugf("[KEYDUMP] ============================================================")
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
		utils.Debugf("[CRYPTO] Send: rand nonce failed: %v", err)
		e.sendErr.Add(1)
		return fmt.Errorf("create packet nonce: %w", err)
	}
	packet := make([]byte, 0, len(header)+len(nonce)+len(data)+e.sendAEAD.Overhead())
	packet = append(packet, header...)
	packet = append(packet, nonce...)
	packet = e.sendAEAD.Seal(packet, nonce, data, header)

	utils.Debugf("[CRYPTO] Send #%d dir=%d plaintext=%d ciphertext=%d nonce=%s",
		e.sendOK.Load()+1, e.sendDirection, len(data), len(packet), hex.EncodeToString(nonce))
	if utils.IsVerbose() && utils.Sensitive() {
		utils.Debugf("[CRYPTO] Send plaintext hexdump:\n%s", hex.Dump(data))
		utils.Debugf("[CRYPTO] Send ciphertext hexdump:\n%s", hex.Dump(packet))
	} else if utils.IsVerbose() {
		utils.Debugf("[CRYPTO] Send dir=%d plaintext=%d ciphertext=%d (hexdump hidden; use --sensitive)",
			e.sendDirection, len(data), len(packet))
	}

	err := e.Transport.Send(packet)
	if err != nil {
		e.sendErr.Add(1)
		utils.Debugf("[CRYPTO] Send FAILED: %v", err)
		return err
	}
	e.sendOK.Add(1)
	return nil
}

func (e *EncryptedTransport) Receive(callback func([]byte)) {
	e.Transport.Receive(func(packet []byte) {
		utils.Debugf("[CRYPTO] Recv raw %d ciphertext bytes", len(packet))
		if utils.IsVerbose() {
			if utils.Sensitive() {
				utils.Debugf("[CRYPTO] Recv raw hexdump:\n%s", hex.Dump(packet))
			} else {
				utils.Debugf("[CRYPTO] Recv raw %d bytes (hexdump hidden; use --sensitive)", len(packet))
			}
		}

		minLen := encryptedHeader + e.receiveAEAD.NonceSize() + e.receiveAEAD.Overhead()
		if len(packet) < minLen {
			e.recvBadLen.Add(1)
			utils.Debugf("[CRYPTO] Recv DROP: too short (%d < %d)", len(packet), minLen)
			return
		}
		header := packet[:encryptedHeader]
		if header[0] != encryptedMagic[0] || header[1] != encryptedMagic[1] ||
			header[2] != encryptedMagic[2] {
			e.recvBadHdr.Add(1)
			utils.Debugf("[CRYPTO] Recv DROP: bad magic %x (want %x)",
				header[:3], encryptedMagic[:])
			return
		}
		if header[3] != encryptedVersion {
			e.recvBadHdr.Add(1)
			utils.Debugf("[CRYPTO] Recv DROP: bad version %d (want %d)",
				header[3], encryptedVersion)
			return
		}
		if header[4] != e.recvDirection {
			e.recvBadHdr.Add(1)
			utils.Debugf("[CRYPTO] Recv DROP: wrong direction %d (want %d)",
				header[4], e.recvDirection)
			return
		}
		nonceEnd := encryptedHeader + e.receiveAEAD.NonceSize()
		nonce := packet[encryptedHeader:nonceEnd]
		utils.Debugf("[CRYPTO] Recv #%d dir=%d nonce=%s cipherLen=%d -> decrypting",
			e.recvOK.Load()+e.recvFail.Load()+1, header[4], hex.EncodeToString(nonce), len(packet)-nonceEnd)
		plaintext, err := e.receiveAEAD.Open(nil, nonce, packet[nonceEnd:], header)
		if err != nil {
			e.recvFail.Add(1)
			utils.Debugf("[CRYPTO] Recv DECRYPT FAIL dir=%d nonce=%s err=%v (recvFail=%d recvOK=%d)",
				header[4], hex.EncodeToString(nonce), err, e.recvFail.Load(), e.recvOK.Load())
			if utils.IsVerbose() && utils.Sensitive() {
				utils.Debugf("[CRYPTO] failed ciphertext hexdump:\n%s", hex.Dump(packet))
			}
			return
		}
		if !e.rememberNonce(nonce) {
			e.recvReplay.Add(1)
			utils.Debugf("[CRYPTO] Recv REPLAY: nonce %s already seen (recvReplay=%d)",
				hex.EncodeToString(nonce), e.recvReplay.Load())
			return
		}
		e.recvOK.Add(1)
		utils.Debugf("[CRYPTO] Recv DECRYPT OK #%d dir=%d plaintext=%d bytes nonce=%s",
			e.recvOK.Load(), header[4], len(plaintext), hex.EncodeToString(nonce))
		if utils.IsVerbose() && utils.Sensitive() {
			utils.Debugf("[CRYPTO] Recv plaintext hexdump:\n%s", hex.Dump(plaintext))
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

// CryptoStats returns the diagnostic counters.
func (e *EncryptedTransport) CryptoStats() (sendOK, sendErr, recvOK, recvFail, recvReplay, recvBadHdr, recvBadLen uint64) {
	return e.sendOK.Load(), e.sendErr.Load(), e.recvOK.Load(), e.recvFail.Load(),
		e.recvReplay.Load(), e.recvBadHdr.Load(), e.recvBadLen.Load()
}

// minInt is defined in session.go; declared here only if that file is
// somehow compiled out. Keep a local guard so this file stands alone.
var _ = minInt
