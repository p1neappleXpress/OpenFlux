package transport

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/flynn/noise"

	"openflux/utils"
)

// EncryptedTransport wraps a raw Transport with authenticated encryption
// negotiated by a Noise NKpsk0 handshake (X25519, AES-256-GCM, SHA-256).
//
// The exit node (responder) owns a static key pair; the client (initiator)
// knows the exit's public key and is otherwise anonymous, unless both sides
// share a pre-shared key, which then also authorizes the client. Every
// handshake produces fresh session keys, so a later compromise of the static
// key or the PSK does not expose recorded traffic.
//
// Sessions rotate on WireGuard's schedule: the initiator re-handshakes once
// a session is rekeyAfterTime old (or after keepaliveTimeout+rekeyTimeout of
// silence following its own sends, which is how an exit restart shows up),
// while the old session keeps working until the new one is confirmed, and
// both sides still accept packets on it until rejectAfterTime.
//
// Frames on the wire (all integers big-endian):
//
//	OFX 0x02 0x01 | sender index u32 | Noise message (48 B)        handshake init
//	OFX 0x02 0x02 | sender u32 | receiver u32 | Noise message (48 B)  handshake response
//	OFX 0x02 0x03 | receiver u32 | counter u64 | ciphertext + tag     data
//
// The counter is the AEAD nonce; the 17-byte data header is the associated
// data. A frame of any other shape, including the v1 format, is dropped.
type EncryptedTransport struct {
	Transport
	cfg EncryptedConfig
	now func() time.Time

	mu       sync.Mutex
	sessions map[uint32]*session // every session that may still receive, by local index
	current  *session            // the session used for sending
	previous *session            // kept for packets in flight after a rekey
	pending  *handshake          // initiator only: the handshake in progress
	staged   [][]byte            // initiator only: packets waiting for a session

	cbMu sync.RWMutex
	cb   func([]byte)

	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// EncryptedConfig configures one side of an encrypted transport.
type EncryptedConfig struct {
	// Initiator is true on the client, which starts handshakes; the exit
	// node only answers them.
	Initiator bool
	// StaticKey is the responder's own key pair (responder only).
	StaticKey noise.DHKey
	// PeerStatic is the responder's public key (initiator only).
	PeerStatic []byte
	// PSK is an optional 32-byte pre-shared key. Both sides must agree on
	// it; nil means the all-zero key, i.e. no client authorization.
	PSK []byte
	// Now overrides the clock (tests).
	Now func() time.Time
}

const (
	encryptedVersion = byte(2)
	encryptedHeader  = 5 // magic + version + type

	msgHandshakeInit = byte(1)
	msgHandshakeResp = byte(2)
	msgData          = byte(3)

	noiseMsgSize      = 32 + 16 // ephemeral public key + tag of the empty payload
	handshakeInitSize = encryptedHeader + 4 + noiseMsgSize
	handshakeRespSize = encryptedHeader + 4 + 4 + noiseMsgSize
	dataHeaderSize    = encryptedHeader + 4 + 8
	aeadOverhead      = 16

	// Session timers, as in WireGuard.
	rekeyAfterTime   = 120 * time.Second // initiator starts a new handshake past this age
	rejectAfterTime  = 180 * time.Second // nobody uses a session past this age
	rekeyTimeout     = 5 * time.Second   // initiation retransmit interval
	rekeyAttemptTime = 90 * time.Second  // stop retransmitting after this
	keepaliveTimeout = 10 * time.Second  // with rekeyTimeout: silence after our sends that means the peer is gone

	rekeyAfterMessages  = uint64(1) << 60
	rejectAfterMessages = ^uint64(0) - (1 << 13)

	// maxStagedPackets bounds what the initiator holds back while it has no
	// session; beyond that the oldest packet is dropped and the tunnel's TCP
	// retransmits it.
	maxStagedPackets = 64
	// maxResponderSessions bounds the sessions the responder keeps, so a
	// flood of initiations from anyone who can write to the channel costs
	// bounded memory and never displaces the session in use.
	maxResponderSessions = 8
)

var (
	encryptedMagic = [3]byte{'O', 'F', 'X'}
	noisePrologue  = []byte("OpenFlux encrypted transport v2")
	zeroPSK        = make([]byte, 32)

	errNoSession        = errors.New("encrypted transport: no session with the peer")
	errSessionExhausted = errors.New("encrypted transport: session counter exhausted")
)

// session is one negotiated pair of directional keys.
type session struct {
	localIndex  uint32
	remoteIndex uint32
	created     time.Time

	sendMu sync.Mutex
	send   *noise.CipherState

	recvMu sync.Mutex
	recv   *noise.CipherState
	replay replayFilter

	// Guarded by EncryptedTransport.mu.
	//
	// confirmed is set when the session may be used for sending: at once
	// on the initiator, on the responder only after the first data packet
	// proves the initiator holds the keys.
	confirmed bool
	// unansweredSince is when we first sent on this session without having
	// heard back since; zero once the peer answers.
	unansweredSince time.Time
	// peerWaiting and lastRecvAt drive the passive keepalive: peerWaiting is
	// set by every authenticated packet from the peer and cleared by any
	// packet we send. A side that stays silent for keepaliveTimeout after a
	// receive sends an empty frame, so the peer's silence detection does not
	// fire on a session that is merely idle.
	peerWaiting bool
	lastRecvAt  time.Time
}

// handshake is the initiator's in-flight handshake. The Noise state is
// rebuilt from the ephemeral seed for every response candidate, so a forged
// response cannot poison it.
type handshake struct {
	seed       [staticKeySize]byte // the ephemeral private key, fed to Noise as its randomness
	localIndex uint32
	packet     []byte // the initiation frame, retransmitted verbatim
	started    time.Time
	lastSent   time.Time
}

// NewEncryptedTransport wraps inner. The initiator needs PeerStatic, the
// responder StaticKey; PSK is optional on both.
func NewEncryptedTransport(inner Transport, cfg EncryptedConfig) (*EncryptedTransport, error) {
	if inner == nil {
		return nil, errors.New("inner transport is nil")
	}
	if cfg.Initiator {
		if len(cfg.PeerStatic) != staticKeySize {
			return nil, fmt.Errorf("initiator needs the peer's %d-byte public key", staticKeySize)
		}
	} else if len(cfg.StaticKey.Private) != staticKeySize || len(cfg.StaticKey.Public) != staticKeySize {
		return nil, errors.New("responder needs its own static key pair")
	}
	if cfg.PSK != nil && len(cfg.PSK) != 32 {
		return nil, errors.New("pre-shared key must be 32 bytes")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	e := &EncryptedTransport{
		Transport: inner,
		cfg:       cfg,
		now:       cfg.Now,
		sessions:  make(map[uint32]*session),
		stop:      make(chan struct{}),
	}
	inner.Receive(e.handleFrame)
	return e, nil
}

// Start starts the inner transport and the timer that drives handshake
// retransmits and keepalives. The initiator also opens a first handshake so
// the session is ready before the first packet needs it.
func (e *EncryptedTransport) Start() error {
	if err := e.Transport.Start(); err != nil {
		return err
	}
	e.wg.Add(1)
	go e.run()
	if e.cfg.Initiator {
		now := e.now()
		e.mu.Lock()
		init := e.ensureHandshakeLocked(now)
		e.mu.Unlock()
		if init != nil {
			_ = e.Transport.Send(init)
		}
	}
	return nil
}

// Stop ends the handshake timer and stops the inner transport.
func (e *EncryptedTransport) Stop() error {
	e.stopOnce.Do(func() { close(e.stop) })
	e.wg.Wait()
	return e.Transport.Stop()
}

func (e *EncryptedTransport) run() {
	defer e.wg.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-e.stop:
			return
		case <-ticker.C:
			e.tick()
		}
	}
}

// tick retransmits the pending initiation every rekeyTimeout and gives up
// after rekeyAttemptTime (the next packet to send then starts over), and
// sends a keepalive when the peer's data went unanswered for keepaliveTimeout.
func (e *EncryptedTransport) tick() {
	now := e.now()
	e.mu.Lock()
	var resend []byte
	if p := e.pending; p != nil {
		switch {
		case now.Sub(p.started) >= rekeyAttemptTime:
			e.pending = nil
			e.staged = nil
		case now.Sub(p.lastSent) >= rekeyTimeout:
			p.lastSent = now
			resend = p.packet
		}
	}
	var keepalive *session
	if s := e.current; s != nil && s.peerWaiting && now.Sub(s.lastRecvAt) >= keepaliveTimeout {
		s.peerWaiting = false
		keepalive = s
	}
	e.mu.Unlock()

	if resend != nil {
		if err := e.Transport.Send(resend); err != nil {
			utils.Debugf("[CRYPTO] retransmit handshake initiation: %v", err)
		}
	}
	if keepalive != nil {
		frame, err := keepalive.seal(nil)
		if err == nil {
			err = e.Transport.Send(frame)
		}
		if err != nil {
			utils.Debugf("[CRYPTO] send keepalive: %v", err)
		}
	}
}

// Receive registers the callback for decrypted packets.
func (e *EncryptedTransport) Receive(callback func([]byte)) {
	e.cbMu.Lock()
	e.cb = callback
	e.cbMu.Unlock()
}

func (e *EncryptedTransport) deliver(data []byte) {
	e.cbMu.RLock()
	cb := e.cb
	e.cbMu.RUnlock()
	if cb != nil {
		cb(data)
	}
}

// Send encrypts one packet. On the initiator a packet sent before a session
// exists is held back and flushed when the handshake completes.
func (e *EncryptedTransport) Send(data []byte) error {
	if e.cfg.Initiator {
		return e.sendInitiator(data)
	}
	return e.sendResponder(data)
}

func (e *EncryptedTransport) sendInitiator(data []byte) error {
	now := e.now()
	e.mu.Lock()
	e.expireLocked(now)
	s := e.current
	if s == nil {
		e.stageLocked(data)
		init := e.ensureHandshakeLocked(now)
		e.mu.Unlock()
		if init != nil {
			return e.Transport.Send(init)
		}
		return nil
	}
	var init []byte
	if e.needsRekeyLocked(s, now) {
		init = e.ensureHandshakeLocked(now)
	}
	if s.unansweredSince.IsZero() {
		s.unansweredSince = now
	}
	s.peerWaiting = false
	e.mu.Unlock()

	if init != nil {
		if err := e.Transport.Send(init); err != nil {
			utils.Debugf("[CRYPTO] send handshake initiation: %v", err)
		}
	}
	frame, err := s.seal(data)
	if err != nil {
		return err
	}
	return e.Transport.Send(frame)
}

func (e *EncryptedTransport) sendResponder(data []byte) error {
	now := e.now()
	e.mu.Lock()
	e.expireLocked(now)
	s := e.current
	if s != nil {
		s.peerWaiting = false
	}
	e.mu.Unlock()
	if s == nil {
		return errNoSession
	}
	frame, err := s.seal(data)
	if err != nil {
		return err
	}
	return e.Transport.Send(frame)
}

// needsRekeyLocked reports whether the initiator should start a handshake
// while still sending on s.
func (e *EncryptedTransport) needsRekeyLocked(s *session, now time.Time) bool {
	if now.Sub(s.created) >= rekeyAfterTime || s.sendCounter() >= rekeyAfterMessages {
		return true
	}
	return !s.unansweredSince.IsZero() && now.Sub(s.unansweredSince) >= keepaliveTimeout+rekeyTimeout
}

// expireLocked forgets sessions nobody may use any more.
func (e *EncryptedTransport) expireLocked(now time.Time) {
	for idx, s := range e.sessions {
		if now.Sub(s.created) < rejectAfterTime && s.sendCounter() < rejectAfterMessages {
			continue
		}
		delete(e.sessions, idx)
		if e.current == s {
			e.current = nil
		}
		if e.previous == s {
			e.previous = nil
		}
	}
}

func (e *EncryptedTransport) stageLocked(data []byte) {
	if len(e.staged) >= maxStagedPackets {
		e.staged = e.staged[1:]
	}
	e.staged = append(e.staged, append([]byte(nil), data...))
}

// ensureHandshakeLocked starts a handshake unless one is in progress and
// returns the initiation frame to send, or nil.
func (e *EncryptedTransport) ensureHandshakeLocked(now time.Time) []byte {
	if p := e.pending; p != nil {
		if now.Sub(p.started) < rekeyAttemptTime {
			return nil
		}
		e.pending = nil
	}
	var seed [staticKeySize]byte
	if _, err := rand.Read(seed[:]); err != nil {
		utils.Debugf("[CRYPTO] generate ephemeral key: %v", err)
		return nil
	}
	hs, err := e.newHandshakeState(seed[:])
	if err != nil {
		utils.Debugf("[CRYPTO] handshake state: %v", err)
		return nil
	}
	msg, _, _, err := hs.WriteMessage(nil, nil)
	if err != nil {
		utils.Debugf("[CRYPTO] write handshake initiation: %v", err)
		return nil
	}
	idx := e.newLocalIndexLocked()
	frame := make([]byte, 0, handshakeInitSize)
	frame = appendHeader(frame, msgHandshakeInit)
	frame = binary.BigEndian.AppendUint32(frame, idx)
	frame = append(frame, msg...)
	e.pending = &handshake{
		seed:       seed,
		localIndex: idx,
		packet:     frame,
		started:    now,
		lastSent:   now,
	}
	return frame
}

// newHandshakeState builds the Noise state for this side. The initiator
// passes the seed its ephemeral key is derived from, so the same initiation
// can be re-derived later; nil lets Noise draw a fresh ephemeral key.
func (e *EncryptedTransport) newHandshakeState(seed []byte) (*noise.HandshakeState, error) {
	psk := e.cfg.PSK
	if psk == nil {
		psk = zeroPSK
	}
	cfg := noise.Config{
		CipherSuite:           noiseSuite,
		Pattern:               noise.HandshakeNK,
		Initiator:             e.cfg.Initiator,
		Prologue:              noisePrologue,
		PresharedKey:          psk,
		PresharedKeyPlacement: 0,
	}
	if seed != nil {
		cfg.Random = bytes.NewReader(seed)
	}
	if e.cfg.Initiator {
		cfg.PeerStatic = e.cfg.PeerStatic
	} else {
		cfg.StaticKeypair = e.cfg.StaticKey
	}
	return noise.NewHandshakeState(cfg)
}

func (e *EncryptedTransport) newLocalIndexLocked() uint32 {
	var b [4]byte
	for {
		if _, err := rand.Read(b[:]); err != nil {
			panic(fmt.Sprintf("crypto/rand: %v", err))
		}
		idx := binary.BigEndian.Uint32(b[:])
		if idx == 0 {
			continue
		}
		if _, taken := e.sessions[idx]; taken {
			continue
		}
		if e.pending != nil && e.pending.localIndex == idx {
			continue
		}
		return idx
	}
}

func appendHeader(frame []byte, typ byte) []byte {
	return append(frame, encryptedMagic[0], encryptedMagic[1], encryptedMagic[2], encryptedVersion, typ)
}

// handleFrame is the inner transport's receive callback.
func (e *EncryptedTransport) handleFrame(frame []byte) {
	if len(frame) < encryptedHeader ||
		frame[0] != encryptedMagic[0] || frame[1] != encryptedMagic[1] || frame[2] != encryptedMagic[2] ||
		frame[3] != encryptedVersion {
		return
	}
	switch frame[4] {
	case msgHandshakeInit:
		if !e.cfg.Initiator {
			e.handleInit(frame)
		}
	case msgHandshakeResp:
		if e.cfg.Initiator {
			e.handleResp(frame)
		}
	case msgData:
		e.handleData(frame)
	}
}

// handleInit answers a handshake initiation with a new, unconfirmed
// session. An initiation that does not verify (wrong exit key, wrong PSK) is
// dropped without a reply.
func (e *EncryptedTransport) handleInit(frame []byte) {
	if len(frame) != handshakeInitSize {
		return
	}
	remoteIndex := binary.BigEndian.Uint32(frame[encryptedHeader:])
	hs, err := e.newHandshakeState(nil)
	if err != nil {
		utils.Debugf("[CRYPTO] handshake state: %v", err)
		return
	}
	if _, _, _, err := hs.ReadMessage(nil, frame[encryptedHeader+4:]); err != nil {
		utils.Debugf("[CRYPTO] rejected handshake initiation: %v", err)
		return
	}
	// Noise Split() hands back (initiator->responder, responder->initiator)
	// on both sides, so the responder receives with the first state and
	// sends with the second.
	msg, recv, send, err := hs.WriteMessage(nil, nil)
	if err != nil {
		utils.Debugf("[CRYPTO] write handshake response: %v", err)
		return
	}
	now := e.now()
	e.mu.Lock()
	e.expireLocked(now)
	for len(e.sessions) >= maxResponderSessions && e.evictLocked() {
	}
	idx := e.newLocalIndexLocked()
	e.sessions[idx] = &session{
		localIndex:  idx,
		remoteIndex: remoteIndex,
		created:     now,
		send:        send,
		recv:        recv,
	}
	e.mu.Unlock()

	utils.Debugf("[CRYPTO] answered handshake from %08x with session %08x (awaiting confirmation)", remoteIndex, idx)
	resp := make([]byte, 0, handshakeRespSize)
	resp = appendHeader(resp, msgHandshakeResp)
	resp = binary.BigEndian.AppendUint32(resp, idx)
	resp = binary.BigEndian.AppendUint32(resp, remoteIndex)
	resp = append(resp, msg...)
	if err := e.Transport.Send(resp); err != nil {
		utils.Debugf("[CRYPTO] send handshake response: %v", err)
	}
}

// handleResp completes the initiator's pending handshake and flushes the
// packets held back meanwhile.
func (e *EncryptedTransport) handleResp(frame []byte) {
	if len(frame) != handshakeRespSize {
		return
	}
	senderIndex := binary.BigEndian.Uint32(frame[encryptedHeader:])
	receiverIndex := binary.BigEndian.Uint32(frame[encryptedHeader+4:])

	e.mu.Lock()
	p := e.pending
	if p == nil || p.localIndex != receiverIndex {
		e.mu.Unlock()
		return
	}
	// Re-derive the handshake state from the stored seed so a response that
	// fails to verify leaves the pending handshake intact.
	hs, err := e.newHandshakeState(p.seed[:])
	if err == nil {
		_, _, _, err = hs.WriteMessage(nil, nil)
	}
	var send, recv *noise.CipherState
	if err == nil {
		_, send, recv, err = hs.ReadMessage(nil, frame[encryptedHeader+8:])
	}
	if err != nil {
		e.mu.Unlock()
		utils.Debugf("[CRYPTO] rejected handshake response: %v", err)
		return
	}
	e.pending = nil
	s := &session{
		localIndex:  p.localIndex,
		remoteIndex: senderIndex,
		created:     e.now(),
		send:        send,
		recv:        recv,
		confirmed:   true,
	}
	e.sessions[s.localIndex] = s
	rekey := e.current != nil
	e.installCurrentLocked(s)
	staged := e.staged
	e.staged = nil
	e.mu.Unlock()

	if rekey {
		utils.Debugf("[CRYPTO] rekeyed: session %08x with peer %08x", s.localIndex, s.remoteIndex)
	} else {
		utils.Debugf("[CRYPTO] session %08x established with peer %08x, flushing %d staged packets", s.localIndex, s.remoteIndex, len(staged))
	}
	for _, data := range staged {
		frame, err := s.seal(data)
		if err != nil {
			return
		}
		if err := e.Transport.Send(frame); err != nil {
			utils.Debugf("[CRYPTO] flush staged packet: %v", err)
		}
	}
}

// evictLocked drops one session to make room on the responder: the oldest
// unconfirmed one first, then a confirmed one that is neither current nor
// previous, then previous. The current session is never evicted. It reports
// whether anything was dropped.
func (e *EncryptedTransport) evictLocked() bool {
	rank := func(s *session) int {
		switch {
		case !s.confirmed:
			return 0
		case s == e.previous:
			return 2
		default:
			return 1
		}
	}
	var victim *session
	for _, s := range e.sessions {
		if s == e.current {
			continue
		}
		if victim == nil || rank(s) < rank(victim) ||
			(rank(s) == rank(victim) && s.created.Before(victim.created)) {
			victim = s
		}
	}
	if victim == nil {
		return false
	}
	delete(e.sessions, victim.localIndex)
	if e.previous == victim {
		e.previous = nil
	}
	return true
}

// installCurrentLocked makes s the sending session and keeps the one it
// replaces as previous, so packets still in flight on it decrypt.
func (e *EncryptedTransport) installCurrentLocked(s *session) {
	if e.current != nil && e.current != s {
		if e.previous != nil && e.previous != e.current {
			delete(e.sessions, e.previous.localIndex)
		}
		e.previous = e.current
	}
	e.current = s
}

// handleData decrypts a data frame for the session it names.
func (e *EncryptedTransport) handleData(frame []byte) {
	if len(frame) < dataHeaderSize+aeadOverhead {
		return
	}
	receiverIndex := binary.BigEndian.Uint32(frame[encryptedHeader:])
	counter := binary.BigEndian.Uint64(frame[encryptedHeader+4:])
	if counter >= rejectAfterMessages {
		return
	}

	now := e.now()
	e.mu.Lock()
	e.expireLocked(now)
	s := e.sessions[receiverIndex]
	e.mu.Unlock()
	if s == nil {
		return
	}
	plaintext, ok := s.open(counter, frame[:dataHeaderSize], frame[dataHeaderSize:])
	if !ok {
		return
	}
	e.mu.Lock()
	s.unansweredSince = time.Time{}
	s.peerWaiting = true
	s.lastRecvAt = now
	confirmed := false
	if !s.confirmed {
		// First authenticated packet from the initiator: it holds the keys,
		// so replies may now go over this session.
		s.confirmed = true
		confirmed = true
		e.installCurrentLocked(s)
	}
	e.mu.Unlock()
	if confirmed {
		utils.Debugf("[CRYPTO] session %08x confirmed by peer %08x, replies now use it", s.localIndex, s.remoteIndex)
	}
	if len(plaintext) == 0 {
		return // keepalive: it only had to authenticate
	}
	e.deliver(plaintext)
}

// sendCounter is the nonce the next data frame would use.
func (s *session) sendCounter() uint64 {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.send.Nonce()
}

// seal encrypts data into a data frame, using the send counter as nonce.
func (s *session) seal(data []byte) ([]byte, error) {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	counter := s.send.Nonce()
	if counter >= rejectAfterMessages {
		return nil, errSessionExhausted
	}
	var header [dataHeaderSize]byte
	copy(header[:], appendHeader(header[:0], msgData))
	binary.BigEndian.PutUint32(header[encryptedHeader:], s.remoteIndex)
	binary.BigEndian.PutUint64(header[encryptedHeader+4:], counter)
	frame := make([]byte, dataHeaderSize, dataHeaderSize+len(data)+aeadOverhead)
	copy(frame, header[:])
	frame, err := s.send.Encrypt(frame, header[:], data)
	if err != nil {
		return nil, fmt.Errorf("encrypt: %w", err)
	}
	return frame, nil
}

// open decrypts a data frame received on this session. The replay window
// is consulted only after the frame authenticated, so a forged counter
// cannot burn a slot for the genuine packet.
func (s *session) open(counter uint64, header, ciphertext []byte) ([]byte, bool) {
	s.recvMu.Lock()
	defer s.recvMu.Unlock()
	s.recv.SetNonce(counter)
	plaintext, err := s.recv.Decrypt(nil, header, ciphertext)
	if err != nil {
		return nil, false
	}
	if !s.replay.ValidateAndUpdate(counter) {
		return nil, false
	}
	return plaintext, true
}
