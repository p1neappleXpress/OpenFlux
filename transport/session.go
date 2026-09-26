package transport

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"openflux/transport/control"
	"openflux/utils"
)

// Session is one logical session between a client and an exit node.
type Session struct {
	mu sync.Mutex

	local            [32]byte
	peer             [32]byte
	params           PeerParameters
	remote           PeerParameters
	exit             bool
	ready            bool
	started          bool
	stopped          bool
	sequence         uint64
	highest          uint64
	window           uint64
	handshakeTimeout time.Duration

	helloInterval time.Duration
	restartMin    time.Duration
	restartMax    time.Duration

	keepaliveInterval time.Duration
	linkTimeout       time.Duration
	peerKeepalive     bool
	noPong            bool

	candidate     *candidatePeer
	candidateLast time.Time

	links map[string]*transportLink
	order []string

	dataCallback    func([]byte)
	controlCallback ControlHandler

	done chan struct{}
	once sync.Once
	wg   sync.WaitGroup

	// Counters for diagnostics.
	cntHelloSent   atomic.Uint64
	cntHelloRecv   atomic.Uint64
	cntHelloAccept atomic.Uint64
	cntHelloReject atomic.Uint64
	cntDataSent    atomic.Uint64
	cntDataRecv    atomic.Uint64
	cntDataDrop    atomic.Uint64
	cntCtrlSent    atomic.Uint64
	cntCtrlRecv    atomic.Uint64
	cntDecodeErr   atomic.Uint64
	cntUnknownKind atomic.Uint64
}

type candidatePeer struct {
	sender  [32]byte
	local   [32]byte
	expires time.Time
}

const (
	candidateTTL      = 20 * time.Second
	candidateInterval = time.Second
)

type transportLink struct {
	name      string
	raw       Transport
	encrypted *EncryptedTransport
	batched   *BatchedTransport
	priority  int

	started   bool
	lastHeard time.Time
	dead      bool
}

func NewSession(p PeerParameters, exit bool) (*Session, error) {
	if !validParameters(p) {
		return nil, errors.New("session requires IPv4/TCP and a packet limit from 1280 to 65000")
	}
	s := &Session{
		params:           p,
		exit:             exit,
		links:            make(map[string]*transportLink),
		done:             make(chan struct{}),
		handshakeTimeout: 20 * time.Second,
		helloInterval:    250 * time.Millisecond,
		restartMin:       time.Second,
		restartMax:       30 * time.Second,

		keepaliveInterval: 10 * time.Second,
		linkTimeout:       30 * time.Second,
	}
	if _, err := rand.Read(s.local[:]); err != nil {
		return nil, err
	}
	side := "CLIENT"
	if exit {
		side = "EXIT"
	}
	utils.Debugf("[SESSION] created side=%s local=%s params.caps=0x%x params.maxPacket=%d",
		side, shortID(s.local), p.Capabilities, p.MaxPacketSize)
	return s, nil
}

func validParameters(p PeerParameters) bool {
	const allowed = control.CapabilityIPv4 | control.CapabilityTCP |
		control.CapabilityUDP | control.CapabilityICMPErrors
	return p.Capabilities&^allowed == 0 &&
		p.Capabilities&(control.CapabilityIPv4|control.CapabilityTCP) ==
			control.CapabilityIPv4|control.CapabilityTCP &&
		p.MaxPacketSize >= 1280 && p.MaxPacketSize <= MaxNegotiatedPacket
}

func (s *Session) AddTransport(name string, raw Transport, secret, context string, priority int) error {
	if raw == nil {
		return errors.New("session: nil transport")
	}
	if name == "" {
		return errors.New("session: empty transport name")
	}

	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("session: cannot add transport after Start")
	}
	if _, exists := s.links[name]; exists {
		s.mu.Unlock()
		return fmt.Errorf("session: transport %q already added", name)
	}
	s.mu.Unlock()

	enc, err := NewEncryptedTransport(raw, secret, context, s.exit)
	if err != nil {
		return fmt.Errorf("session: wrap %q: %w", name, err)
	}
	bat := NewBatchedTransport(enc)

	link := &transportLink{
		name:      name,
		raw:       raw,
		encrypted: enc,
		batched:   bat,
		priority:  priority,
	}

	s.mu.Lock()
	s.links[name] = link
	s.order = insertByPriority(s.order, name, link.priority, s.links)
	s.mu.Unlock()
	utils.Debugf("[SESSION] AddTransport name=%q type=%T priority=%d", name, raw, priority)
	return nil
}

func (s *Session) RemoveTransport(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	link, ok := s.links[name]
	if !ok {
		return fmt.Errorf("session: transport %q not found", name)
	}
	delete(s.links, name)
	for i, n := range s.order {
		if n == name {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	_ = link.batched.Stop()
	return nil
}

func insertByPriority(order []string, name string, priority int, links map[string]*transportLink) []string {
	pos := len(order)
	for i, n := range order {
		if links[n].priority < priority {
			pos = i
			break
		}
	}
	out := make([]string, 0, len(order)+1)
	out = append(out, order[:pos]...)
	out = append(out, name)
	out = append(out, order[pos:]...)
	return out
}

func (s *Session) Start() error {
	s.mu.Lock()
	if s.stopped || s.started {
		s.mu.Unlock()
		return errors.New("session already started or stopped")
	}
	if len(s.order) == 0 {
		s.mu.Unlock()
		return errors.New("session: no transports added")
	}
	s.started = true
	links := make([]*transportLink, 0, len(s.order))
	for _, name := range s.order {
		links = append(links, s.links[name])
	}
	exit := s.exit
	timeout := s.handshakeTimeout
	local := s.local
	order := append([]string(nil), s.order...)
	s.mu.Unlock()

	role := "client"
	if exit {
		role = "exit"
	}
	utils.Debugf("[SESSION] Start role=%s timeout=%v local=%s transports=%v",
		role, timeout, shortID(local), order)

	for _, link := range links {
		if err := s.startLink(link); err != nil {
			utils.Debugf("[SESSION] transport %q start: %v; retrying in background", link.name, err)
			s.superviseLink(link)
		} else {
			utils.Debugf("[SESSION] transport %q started (priority=%d)", link.name, link.priority)
		}
	}
	s.wg.Add(1)
	go s.keepaliveLoop()
	if exit {
		utils.Debugf("[SESSION] exit: Start returning, waiting for a client")
		return nil
	}

	s.wg.Add(1)
	go s.helloLoop()

	utils.Debugf("[SESSION] client: waiting for handshake (timeout=%v)", timeout)
	if err := s.waitReady(timeout); err != nil {
		utils.Debugf("[SESSION] handshake FAILED after %v: %v", timeout, err)
		s.dumpDiagnostics("handshake-failure")
		_ = s.Stop()
		return fmt.Errorf("session: handshake failed: %w", err)
	}

	s.mu.Lock()
	peer := s.peer
	remote := s.remote
	s.mu.Unlock()
	utils.Debugf("[SESSION] handshake OK: peer=%s caps=0x%x maxPacket=%d",
		shortID(peer), remote.Capabilities, remote.MaxPacketSize)
	s.dumpDiagnostics("handshake-success")
	return nil
}

func (s *Session) dumpDiagnostics(why string) {
	utils.Debugf("[SESSION-DIAG] reason=%s", why)
	utils.Debugf("[SESSION-DIAG] helloSent=%d helloRecv=%d helloAccept=%d helloReject=%d",
		s.cntHelloSent.Load(), s.cntHelloRecv.Load(), s.cntHelloAccept.Load(), s.cntHelloReject.Load())
	utils.Debugf("[SESSION-DIAG] dataSent=%d dataRecv=%d dataDrop=%d",
		s.cntDataSent.Load(), s.cntDataRecv.Load(), s.cntDataDrop.Load())
	utils.Debugf("[SESSION-DIAG] ctrlSent=%d ctrlRecv=%d decodeErr=%d unknownKind=%d",
		s.cntCtrlSent.Load(), s.cntCtrlRecv.Load(), s.cntDecodeErr.Load(), s.cntUnknownKind.Load())
	s.mu.Lock()
	for name, l := range s.links {
		utils.Debugf("[SESSION-DIAG] link=%q started=%v dead=%v lastHeard=%v raw.IsConnected=%v",
			name, l.started, l.dead, time.Since(l.lastHeard).Round(time.Millisecond), l.raw.IsConnected())
		if l.encrypted != nil {
			so, se, ro, rf, rr, bh, bl := l.encrypted.CryptoStats()
			utils.Debugf("[SESSION-DIAG]   crypto link=%q sendOK=%d sendErr=%d recvOK=%d recvFail=%d recvReplay=%d badHdr=%d badLen=%d",
				name, so, se, ro, rf, rr, bh, bl)
		}
	}
	s.mu.Unlock()
}

func (s *Session) startLink(link *transportLink) error {
	if err := link.batched.Start(); err != nil {
		return err
	}
	link.batched.Receive(func(p []byte) { s.receive(link, p) })

	s.mu.Lock()
	stopped := s.stopped
	if !stopped {
		link.started = true
	}
	s.mu.Unlock()
	if stopped {
		_ = link.batched.Stop()
		_ = link.raw.Stop()
		return errors.New("session stopped")
	}
	return nil
}

func (s *Session) superviseLink(link *transportLink) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		delay := s.restartMin
		attempt := 0
		for {
			select {
			case <-s.done:
				return
			case <-time.After(delay):
			}
			s.mu.Lock()
			current := s.links[link.name] == link
			s.mu.Unlock()
			if !current {
				return
			}
			attempt++
			err := s.startLink(link)
			if err == nil {
				utils.Debugf("[SESSION] transport %q up after %d retries", link.name, attempt)
				return
			}
			utils.Debugf("[SESSION] transport %q start retry #%d: %v", link.name, attempt, err)
			if delay *= 2; delay > s.restartMax {
				delay = s.restartMax
			}
		}
	}()
}

func (s *Session) helloLoop() {
	defer s.wg.Done()
	tick := time.NewTicker(s.helloInterval)
	defer tick.Stop()
	attempt := 0
	for {
		s.mu.Lock()
		ready := s.ready
		var names []string
		if !ready {
			for _, name := range s.order {
				if s.links[name].started {
					names = append(names, name)
				}
			}
		}
		local := s.local
		peer := s.peer
		s.mu.Unlock()
		if !ready && len(names) > 0 {
			attempt++
			if attempt == 1 || attempt%20 == 0 {
				utils.Debugf("[SESSION] hello #%d via %v (local=%s peer=%s)",
					attempt, names, shortID(local), shortID(peer))
			}
		}
		for _, name := range names {
			if err := s.helloVia(name); err != nil {
				utils.Debugf("[SESSION] hello via %q: %v", name, err)
			}
		}
		select {
		case <-s.done:
			return
		case <-tick.C:
		}
	}
}

func (s *Session) keepaliveLoop() {
	defer s.wg.Done()
	tick := time.NewTicker(s.keepaliveInterval)
	defer tick.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-tick.C:
		}
		s.mu.Lock()
		if !s.exit && s.ready && s.peerKeepalive && s.peerSilentLocked() {
			s.resetLocked()
			utils.Debugf("[SESSION] peer silent on every transport; handshaking again")
		}
		var quiet []*transportLink
		if s.ready {
			for _, name := range s.order {
				l := s.links[name]
				if l.started && !l.dead && time.Since(l.lastHeard) >= s.keepaliveInterval {
					quiet = append(quiet, l)
				}
			}
		}
		s.mu.Unlock()
		for _, l := range quiet {
			if err := s.sendControlVia(l, control.SubtypeLinkPing, nil); err != nil {
				utils.Debugf("[SESSION] LinkPing via %q: %v", l.name, err)
			} else {
				utils.Debugf("[SESSION] LinkPing -> %q", l.name)
			}
		}
	}
}

func (s *Session) peerSilentLocked() bool {
	for _, l := range s.links {
		if time.Since(l.lastHeard) < s.linkTimeout {
			return false
		}
	}
	return true
}

func (s *Session) resetLocked() {
	s.ready = false
	s.peer = [32]byte{}
	s.remote = PeerParameters{}
	s.sequence, s.highest, s.window = 0, 0, 0
	s.peerKeepalive = false
	s.candidate = nil
	if _, err := rand.Read(s.local[:]); err != nil {
		utils.Debugf("[SESSION] new challenge: %v", err)
	}
}

func (s *Session) waitReady(timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	lastLog := time.Now()
	for !s.IsConnected() {
		select {
		case <-s.done:
			return errors.New("session stopped")
		case <-timer.C:
			return errors.New("handshake timed out")
		case <-tick.C:
			if time.Since(lastLog) >= 5*time.Second {
				s.mu.Lock()
				state := "waiting"
				if s.ready {
					state = "ready-but-no-live-link"
				}
				s.mu.Unlock()
				utils.Debugf("[SESSION] handshake still pending (%s)", state)
				lastLog = time.Now()
			}
		}
	}
	return nil
}

func (s *Session) Stop() error {
	s.once.Do(func() {
		s.mu.Lock()
		s.stopped = true
		s.ready = false
		close(s.done)
		links := make([]*transportLink, 0, len(s.links))
		for _, l := range s.links {
			links = append(links, l)
		}
		s.mu.Unlock()
		for _, l := range links {
			_ = l.batched.Stop()
			_ = l.raw.Stop()
		}
	})
	s.wg.Wait()
	return nil
}

func (s *Session) IsConnected() bool {
	s.mu.Lock()
	ready := s.ready && !s.stopped
	s.mu.Unlock()
	if !ready {
		return false
	}
	return s.anyLive()
}

func (s *Session) anyLive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.liveLinksLocked()) > 0
}

func (s *Session) liveLocked(l *transportLink) bool {
	if l == nil || l.dead || !l.started || !l.raw.IsConnected() {
		return false
	}
	return !s.peerKeepalive || time.Since(l.lastHeard) < s.linkTimeout
}

func (s *Session) ActiveTransport() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready || s.stopped {
		return ""
	}
	if links := s.liveLinksLocked(); len(links) > 0 {
		return links[0].name
	}
	return ""
}

func (s *Session) IsExit() bool { return s.exit }

func (s *Session) PeerParameters() (PeerParameters, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.remote, s.ready && !s.stopped
}

func (s *Session) Transports() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.order...)
}

func (s *Session) Receive(cb func([]byte)) {
	s.mu.Lock()
	s.dataCallback = cb
	s.mu.Unlock()
}

func (s *Session) SetControlHandler(h ControlHandler) {
	s.mu.Lock()
	s.controlCallback = h
	s.mu.Unlock()
}

// ---- hello ----

func (s *Session) buildHello() *control.Envelope {
	env := &control.Envelope{
		Kind:  control.KindHello,
		Role:  s.roleLocked(),
		Local: s.local,
		Peer:  s.peer,
		Hello: &control.HelloTail{
			Capabilities:  control.Capabilities(s.params.Capabilities),
			MaxPacketSize: uint16(s.params.MaxPacketSize),
		},
	}
	if s.ready {
		env.Hello.Ready = 1
	}
	return env
}

func (s *Session) helloVia(name string) error {
	s.mu.Lock()
	link, ok := s.links[name]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("transport %q not found", name)
	}
	env := s.buildHello()
	local, peer, ready := s.local, s.peer, s.ready
	s.mu.Unlock()

	raw, err := env.Encode()
	if err != nil {
		return err
	}
	n := s.cntHelloSent.Add(1)
	utils.Debugf("[SESSION] hello #%d -> %q role=%d local=%s peer=%s ready=%v size=%d",
		n, name, env.Role, shortID(local), shortID(peer), ready, len(raw))
	if utils.IsVerbose() {
		utils.Debugf("[SESSION] hello hexdump:\n%s", hex.Dump(raw))
	}
	return link.batched.Send(raw)
}

// ---- IPv4 data ----

func (s *Session) Send(p []byte) error {
	s.mu.Lock()
	if !s.ready || s.stopped {
		s.mu.Unlock()
		return ErrNegotiationPending
	}
	if err := permittedPacket(p, s.remote); err != nil {
		s.mu.Unlock()
		return err
	}
	if s.sequence == ^uint64(0) {
		s.mu.Unlock()
		return errors.New("session sequence exhausted; restart both peers")
	}
	s.sequence++
	seq := s.sequence
	env := &control.Envelope{
		Kind:  control.KindIPv4,
		Role:  s.roleLocked(),
		Local: s.local,
		Peer:  s.peer,
		Data:  &control.DataTail{Sequence: seq},
	}
	links := s.liveLinksLocked()
	s.mu.Unlock()

	if len(links) == 0 {
		return errors.New("session: no live transport")
	}

	raw, err := env.Encode()
	if err != nil {
		return err
	}
	raw = append(raw, p...)

	top := links[:1]
	for _, l := range links[1:] {
		if l.priority != links[0].priority {
			break
		}
		top = append(top, l)
	}
	key := extractFlowKeyBytes(p)
	idx := int(flowHashBytes(key) % uint64(len(top)))
	chosen := top[idx]

	n := s.cntDataSent.Add(1)
	if n == 1 || n%100 == 0 {
		utils.Debugf("[SESSION] send IPv4 #%d seq=%d via %q size=%d proto=%d", n, seq, chosen.name, len(p), p[9])
	}
	return chosen.batched.Send(raw)
}

func (s *Session) liveLinksLocked() []*transportLink {
	out := make([]*transportLink, 0, len(s.links))
	for _, name := range s.order {
		if l := s.links[name]; s.liveLocked(l) {
			out = append(out, l)
		}
	}
	return out
}

func (s *Session) SendControl(subtype control.Subtype, payload []byte) error {
	s.mu.Lock()
	links := s.liveLinksLocked()
	s.mu.Unlock()
	if len(links) == 0 {
		return errors.New("session: no live transport for control")
	}
	return s.sendControlVia(links[0], subtype, payload)
}

func (s *Session) sendControlVia(link *transportLink, subtype control.Subtype, payload []byte) error {
	if subtype == 0 {
		return errors.New("session: empty control subtype")
	}
	if len(payload) > 0xffff {
		return fmt.Errorf("session: control payload too large (%d bytes)", len(payload))
	}
	s.mu.Lock()
	if !s.ready || s.stopped {
		s.mu.Unlock()
		return ErrNegotiationPending
	}
	env := &control.Envelope{
		Kind:  control.KindControl,
		Role:  s.roleLocked(),
		Local: s.local,
		Peer:  s.peer,
		Control: &control.ControlTail{
			Subtype:    subtype,
			Flags:      0,
			PayloadLen: uint16(len(payload)),
		},
	}
	s.mu.Unlock()

	raw, err := env.Encode()
	if err != nil {
		return err
	}
	if len(payload) > 0 {
		raw = append(raw, payload...)
	}
	n := s.cntCtrlSent.Add(1)
	utils.Debugf("[SESSION] control #%d -> %q subtype=0x%02x payloadLen=%d size=%d",
		n, link.name, subtype, len(payload), len(raw))
	if utils.IsVerbose() && utils.Sensitive() {
		utils.Debugf("[SESSION] control hexdump:\n%s", hex.Dump(raw))
	}
	return link.batched.Send(raw)
}

func permittedPacket(p []byte, limits PeerParameters) error {
	if len(p) < 20 || p[0]>>4 != 4 ||
		int(p[0]&15)*4 < 20 || int(p[0]&15)*4 > len(p) {
		return errors.New("session: requires complete IPv4 packets")
	}
	if len(p) > limits.MaxPacketSize {
		return fmt.Errorf("IPv4 packet exceeds negotiated maximum %d", limits.MaxPacketSize)
	}
	switch p[9] {
	case 6:
	case 17:
		if limits.Capabilities&control.CapabilityUDP == 0 {
			return errors.New("peer does not support UDP")
		}
	case 1:
		if limits.Capabilities&control.CapabilityICMPErrors == 0 {
			return errors.New("peer does not support ICMP errors")
		}
	default:
		return errors.New("unsupported IP protocol")
	}
	return nil
}

// ---- receive ----

func (s *Session) receive(link *transportLink, p []byte) {
	env, err := control.Decode(p)
	if err != nil {
		s.cntDecodeErr.Add(1)
		utils.Debugf("[SESSION] decode error #%d from %q (%d bytes): %v",
			s.cntDecodeErr.Load(), link.name, len(p), err)
		if utils.IsVerbose() && utils.Sensitive() {
			utils.Debugf("[SESSION] malformed packet hexdump:\n%s", hex.Dump(p))
		}
		return
	}
	if env.Role == s.roleLocked() {
		utils.Debugf("[SESSION] drop from %q: same role %d", link.name, env.Role)
		return
	}
	if utils.IsVerbose() {
		utils.Debugf("[SESSION] recv from %q kind=%d role=%d size=%d local=%s peer=%s",
			link.name, env.Kind, env.Role, len(p), shortID(env.Local), shortID(env.Peer))
	}
	switch env.Kind {
	case control.KindHello:
		s.receiveHello(link, env)
	case control.KindIPv4:
		s.receiveIPv4(link, p, env)
	case control.KindControl:
		s.receiveControl(link, p, env)
	default:
		s.cntUnknownKind.Add(1)
		utils.Debugf("[SESSION] unknown kind 0x%02x #%d from %q, ignoring",
			env.Kind, s.cntUnknownKind.Load(), link.name)
		if utils.IsVerbose() && utils.Sensitive() {
			utils.Debugf("[SESSION] unknown-kind hexdump:\n%s", hex.Dump(p))
		}
	}
}

func (s *Session) roleLocked() control.Role {
	if s.exit {
		return control.RoleExit
	}
	return control.RoleClient
}

func (s *Session) receiveHello(link *transportLink, env *control.Envelope) {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	n := s.cntHelloRecv.Add(1)
	if env.Hello == nil || env.Hello.Reserved != 0 || env.Hello.Ready > 1 {
		s.cntHelloReject.Add(1)
		s.mu.Unlock()
		utils.Debugf("[SESSION] hello #%d from %q REJECT: bad tail", n, link.name)
		return
	}
	sender := env.Local
	if sender == ([32]byte{}) {
		s.cntHelloReject.Add(1)
		s.mu.Unlock()
		utils.Debugf("[SESSION] hello #%d from %q REJECT: zero local", n, link.name)
		return
	}
	params := PeerParameters{
		Capabilities:  control.Capabilities(env.Hello.Capabilities),
		MaxPacketSize: int(env.Hello.MaxPacketSize),
	}
	if !validParameters(params) {
		s.cntHelloReject.Add(1)
		s.mu.Unlock()
		utils.Debugf("[SESSION] hello #%d from %q REJECT: invalid params caps=0x%x maxPacket=%d",
			n, link.name, params.Capabilities, params.MaxPacketSize)
		return
	}
	echo := env.Peer == s.local
	zero := env.Peer == ([32]byte{})

	utils.Debugf("[SESSION] hello #%d from %q sender=%s peer=%s echo=%v zero=%v ready=%v established=%v ourLocal=%s",
		n, link.name, shortID(sender), shortID(env.Peer), echo, zero, env.Hello.Ready == 1, s.ready, shortID(s.local))

	if s.ready && sender != s.peer {
		utils.Debugf("[SESSION] hello from unknown sender %s while established with %s; offering challenge",
			shortID(sender), shortID(s.peer))
		s.offerReplacementLocked(link, sender, params, env)
		return
	}
	if s.ready && (params.Capabilities&s.params.Capabilities != s.remote.Capabilities ||
		minInt(params.MaxPacketSize, s.params.MaxPacketSize) != s.remote.MaxPacketSize) {
		s.cntHelloReject.Add(1)
		s.mu.Unlock()
		utils.Debugf("[SESSION] hello #%d from %q REJECT: params changed while established", n, link.name)
		return
	}
	if !echo && !zero {
		s.cntHelloReject.Add(1)
		s.mu.Unlock()
		utils.Debugf("[SESSION] hello #%d from %q REJECT: stale peer echo", n, link.name)
		return
	}
	changed := sender != s.peer
	wasReady := s.ready
	s.peer = sender
	if echo {
		s.remote = PeerParameters{
			Capabilities:  params.Capabilities & s.params.Capabilities,
			MaxPacketSize: minInt(params.MaxPacketSize, s.params.MaxPacketSize),
		}
		s.ready = true
	}
	link.lastHeard = time.Now()
	var names []string
	if changed || (!wasReady && echo) || (wasReady && env.Hello.Ready == 0) {
		names = append(names, s.order...)
	}
	peer := s.peer
	s.mu.Unlock()

	s.cntHelloAccept.Add(1)
	if echo {
		utils.Debugf("[SESSION] hello #%d from %q ACCEPT: peer=%s -> ready (accept=%d)",
			n, link.name, shortID(peer), s.cntHelloAccept.Load())
	} else {
		utils.Debugf("[SESSION] hello #%d from %q ACCEPT: initial, no echo yet (peer=%s)",
			n, link.name, shortID(peer))
	}
	for _, name := range names {
		if err := s.helloVia(name); err != nil {
			utils.Debugf("[SESSION] hello reply via %q: %v", name, err)
		}
	}
}

func (s *Session) offerReplacementLocked(link *transportLink, sender [32]byte, params PeerParameters, env *control.Envelope) {
	now := time.Now()
	cand := s.candidate
	if cand != nil && now.After(cand.expires) {
		cand, s.candidate = nil, nil
	}

	if cand != nil && cand.sender == sender && env.Peer == cand.local {
		s.local = cand.local
		s.peer = sender
		s.remote = PeerParameters{
			Capabilities:  params.Capabilities & s.params.Capabilities,
			MaxPacketSize: minInt(params.MaxPacketSize, s.params.MaxPacketSize),
		}
		s.sequence, s.highest, s.window = 0, 0, 0
		s.peerKeepalive = false
		s.candidate = nil
		for _, l := range s.links {
			l.lastHeard = time.Time{}
		}
		link.lastHeard = now
		names := append([]string(nil), s.order...)
		s.mu.Unlock()
		utils.Debugf("[SESSION] peer REPLACED by %s after fresh challenge echo", shortID(sender))
		for _, name := range names {
			_ = s.helloVia(name)
		}
		return
	}

	if env.Peer != ([32]byte{}) {
		utils.Debugf("[SESSION] candidate hello from %s rejected: peer echo non-zero", shortID(sender))
		s.mu.Unlock()
		return
	}
	if cand == nil || cand.sender != sender {
		if now.Sub(s.candidateLast) < candidateInterval {
			utils.Debugf("[SESSION] candidate hello from %s rate-limited", shortID(sender))
			s.mu.Unlock()
			return
		}
		cand = &candidatePeer{sender: sender, expires: now.Add(candidateTTL)}
		if _, err := rand.Read(cand.local[:]); err != nil {
			s.mu.Unlock()
			return
		}
		s.candidate = cand
		s.candidateLast = now
		utils.Debugf("[SESSION] new candidate %s: challenge=%s expires=%v",
			shortID(sender), shortID(cand.local), candidateTTL)
	}
	offer := &control.Envelope{
		Kind:  control.KindHello,
		Role:  s.roleLocked(),
		Local: cand.local,
		Peer:  sender,
		Hello: &control.HelloTail{
			Capabilities:  control.Capabilities(s.params.Capabilities),
			MaxPacketSize: uint16(s.params.MaxPacketSize),
		},
	}
	var links []*transportLink
	for _, name := range s.order {
		if l := s.links[name]; l.started {
			links = append(links, l)
		}
	}
	s.mu.Unlock()

	raw, err := offer.Encode()
	if err != nil {
		return
	}
	for _, l := range links {
		_ = l.batched.Send(raw)
	}
}

func (s *Session) receiveIPv4(link *transportLink, p []byte, env *control.Envelope) {
	s.mu.Lock()
	if env.Data == nil {
		s.cntDataDrop.Add(1)
		s.mu.Unlock()
		utils.Debugf("[SESSION] IPv4 from %q: missing data tail", link.name)
		return
	}
	if !s.ready || s.stopped {
		s.cntDataDrop.Add(1)
		s.mu.Unlock()
		utils.Debugf("[SESSION] IPv4 from %q dropped: session not ready/stopped", link.name)
		return
	}
	if env.Local != s.peer || env.Peer != s.local {
		s.cntDataDrop.Add(1)
		s.mu.Unlock()
		utils.Debugf("[SESSION] IPv4 from %q dropped: challenge mismatch", link.name)
		return
	}
	payload := p[control.EnvelopeSize:]
	if err := permittedPacket(payload, s.remote); err != nil {
		s.cntDataDrop.Add(1)
		s.mu.Unlock()
		utils.Debugf("[SESSION] IPv4 from %q dropped: %v", link.name, err)
		return
	}
	if !s.acceptSequenceLocked(env.Data.Sequence) {
		s.cntDataDrop.Add(1)
		s.mu.Unlock()
		utils.Debugf("[SESSION] IPv4 from %q dropped: replay/out-of-window seq=%d (highest=%d)",
			link.name, env.Data.Sequence, s.highest)
		return
	}
	link.lastHeard = time.Now()
	cb := s.dataCallback
	s.mu.Unlock()
	n := s.cntDataRecv.Add(1)
	if n == 1 || n%100 == 0 {
		utils.Debugf("[SESSION] recv IPv4 #%d seq=%d from %q size=%d proto=%d",
			n, env.Data.Sequence, link.name, len(payload), payload[9])
	}
	if cb != nil {
		cb(append([]byte(nil), payload...))
	}
}

func (s *Session) receiveControl(link *transportLink, p []byte, env *control.Envelope) {
	if env.Control == nil || env.Control.Flags != 0 || env.Control.Subtype == 0 {
		utils.Debugf("[SESSION] control from %q: bad tail", link.name)
		return
	}
	s.mu.Lock()
	if !s.ready || s.stopped {
		s.mu.Unlock()
		utils.Debugf("[SESSION] control from %q dropped: session not ready/stopped", link.name)
		return
	}
	if env.Local != s.peer || env.Peer != s.local {
		s.mu.Unlock()
		utils.Debugf("[SESSION] control from %q dropped: challenge mismatch", link.name)
		return
	}
	link.lastHeard = time.Now()
	switch env.Control.Subtype {
	case control.SubtypeLinkPing:
		noPong := s.noPong
		s.mu.Unlock()
		utils.Debugf("[SESSION] LinkPing from %q -> pong", link.name)
		if noPong {
			return
		}
		go func() { _ = s.sendControlVia(link, control.SubtypeLinkPong, nil) }()
		return
	case control.SubtypeLinkPong:
		s.peerKeepalive = true
		s.mu.Unlock()
		utils.Debugf("[SESSION] LinkPong from %q: peer keepalive enabled", link.name)
		return
	}
	sub := env.Control.Subtype
	cb := s.controlCallback
	s.mu.Unlock()
	n := s.cntCtrlRecv.Add(1)
	utils.Debugf("[SESSION] control #%d from %q subtype=0x%02x payloadLen=%d",
		n, link.name, sub, env.Control.PayloadLen)
	if utils.IsVerbose() && utils.Sensitive() {
		utils.Debugf("[SESSION] control payload hexdump:\n%s", hex.Dump(p))
	}
	if cb == nil {
		utils.Debugf("[SESSION] control subtype=0x%02x has no handler, dropping", sub)
		return
	}
	payload := append([]byte(nil), p[control.EnvelopeSize:]...)
	go cb(sub, payload)
}

func (s *Session) acceptSequenceLocked(seq uint64) bool {
	if seq == 0 {
		return false
	}
	if seq > s.highest {
		gap := seq - s.highest
		if gap >= 64 {
			s.window = 0
		} else {
			s.window <<= gap
		}
		s.highest = seq
		s.window |= 1
		return true
	}
	gap := s.highest - seq
	if gap >= 64 || s.window&(uint64(1)<<gap) != 0 {
		return false
	}
	s.window |= uint64(1) << gap
	return true
}

func (s *Session) Stats() TransportStats {
	s.mu.Lock()
	links := make([]*transportLink, 0, len(s.links))
	for _, l := range s.links {
		links = append(links, l)
	}
	s.mu.Unlock()

	var out TransportStats
	for _, l := range links {
		st := l.raw.Stats()
		out.BytesSent += st.BytesSent
		out.BytesReceived += st.BytesReceived
		out.PacketsSent += st.PacketsSent
		out.PacketsRecv += st.PacketsRecv
		out.Reconnects += st.Reconnects
	}
	out.Connected = s.IsConnected()
	return out
}

func (s *Session) MarkDead(name string) {
	s.mu.Lock()
	if l, ok := s.links[name]; ok {
		l.dead = true
	}
	s.mu.Unlock()
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func shortID(id [32]byte) string {
	return hex.EncodeToString(id[:4])
}
