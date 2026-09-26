package transport

import (
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"time"

	"openflux/transport/control"
	"openflux/utils"
)

// Session is one logical session between a client and an exit node.
//
// A Session outlives any particular transport: it holds one handshake, one
// challenge pair, one sequence, and one replay window, while routing IPv4
// packets and control messages across one or more underlying transports.
//
// When the active transport dies, its in-flight flows are lost (TCP will
// retransmit); new flows are routed to whichever live transport is left.
// The handshake is NOT repeated on transport changes.
//
// Each transport is wrapped in its own EncryptedTransport + BatchedTransport
// inside a transportLink. The Session coordinates them.
type Session struct {
	mu sync.Mutex

	// Handshake state, shared across all transports.
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

	// helloInterval paces the client's hellos until the session is ready.
	// restartMin/restartMax bound the backoff for carriers whose Start
	// failed (e.g. a captcha during authorization).
	helloInterval time.Duration
	restartMin    time.Duration
	restartMax    time.Duration

	// A carrier only counts as live if the peer was heard on it within
	// linkTimeout; idle carriers are pinged every keepaliveInterval.
	// peerKeepalive is set by the first pong: a peer that never answers
	// predates keepalive, and liveness falls back to the carrier's own
	// IsConnected. noPong makes this side behave like such a peer (tests).
	keepaliveInterval time.Duration
	linkTimeout       time.Duration
	peerKeepalive     bool
	noPong            bool

	// candidate is a new peer that must prove itself before it replaces an
	// established one; see receiveHello.
	candidate     *candidatePeer
	candidateLast time.Time

	// Transport links, ordered by priority (descending).
	links map[string]*transportLink
	order []string

	// Callbacks.
	dataCallback    func([]byte)
	controlCallback ControlHandler

	done chan struct{}
	once sync.Once
	wg   sync.WaitGroup
}

// candidatePeer is an unknown sender offered a fresh challenge while the
// session is established with someone else.
type candidatePeer struct {
	sender  [32]byte
	local   [32]byte
	expires time.Time
}

const (
	candidateTTL      = 20 * time.Second
	candidateInterval = time.Second
)

// transportLink wraps one raw Transport with its encryption and batching.
type transportLink struct {
	name      string
	raw       Transport
	encrypted *EncryptedTransport
	batched   *BatchedTransport
	priority  int

	// started is set once the carrier is up and its receive path attached.
	started bool

	// lastHeard is when an authenticated envelope from the current peer
	// last arrived on this carrier.
	lastHeard time.Time

	// Set once the link has been observed to fail; it is removed from
	// routing but kept for stats until RemoveTransport is called.
	dead bool
}

// NewSession builds an empty Session. Use AddTransport to attach transports
// before calling Start.
//
// inner must be an *EncryptedTransport; unencrypted sessions are rejected.
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

// AddTransport wraps raw with EncryptedTransport + BatchedTransport and
// attaches it to the Session. name must be unique; priority controls the
// order in which transports are tried during handshake and selected for
// control traffic (higher = preferred).
//
// secret and context are the same values that would be passed to
// NewEncryptedTransport.
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

	return nil
}

// RemoveTransport detaches a transport from the Session.
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

// insertByPriority inserts name into order at the correct position.
// Caller holds s.mu.
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

// Start brings up every transport. A carrier whose Start fails is retried
// in the background with backoff, so e.g. a transport stuck on a captcha
// joins the session once it recovers instead of being dropped for good.
//
// The client keeps sending hellos through every started carrier until the
// session is ready, and returns an error if that takes longer than the
// handshake timeout. The exit node answers hellos but never initiates, so
// its Start returns as soon as the carriers are launched: it must be able
// to sit idle until a client shows up.
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
	s.mu.Unlock()

	for _, link := range links {
		if err := s.startLink(link); err != nil {
			utils.Debugf("[SESSION] transport %q start: %v; retrying in background", link.name, err)
			s.superviseLink(link)
		}
	}
	s.wg.Add(1)
	go s.keepaliveLoop()
	if exit {
		return nil
	}

	s.wg.Add(1)
	go s.helloLoop()
	if err := s.waitReady(timeout); err != nil {
		_ = s.Stop()
		return fmt.Errorf("session: handshake failed: %w", err)
	}
	return nil
}

// startLink starts one carrier and attaches it to the receive path. The
// batched wrapper starts the raw transport through the encryption layer;
// starting raw separately as well would bring the carrier up twice (two
// sessions attached to the same document).
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

// superviseLink retries startLink with exponential backoff until it
// succeeds, the link is removed, or the session stops.
func (s *Session) superviseLink(link *transportLink) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		delay := s.restartMin
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
			err := s.startLink(link)
			if err == nil {
				utils.Debugf("[SESSION] transport %q up after retry", link.name)
				return
			}
			utils.Debugf("[SESSION] transport %q start: %v", link.name, err)
			if delay *= 2; delay > s.restartMax {
				delay = s.restartMax
			}
		}
	}()
}

// helloLoop keeps offering the handshake through every started carrier
// until the session is ready.
func (s *Session) helloLoop() {
	defer s.wg.Done()
	tick := time.NewTicker(s.helloInterval)
	defer tick.Stop()
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
		s.mu.Unlock()
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

// keepaliveLoop pings every carrier the peer has been quiet on, so a
// carrier that stopped reaching the peer ages out of routing.
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
			_ = s.sendControlVia(l, control.SubtypeLinkPing, nil)
		}
	}
}

// peerSilentLocked reports whether the peer has not been heard on any
// carrier within linkTimeout. Caller holds s.mu.
func (s *Session) peerSilentLocked() bool {
	for _, l := range s.links {
		if time.Since(l.lastHeard) < s.linkTimeout {
			return false
		}
	}
	return true
}

// resetLocked drops the established peer and starts over as a new session
// identity, as if the process had restarted: a restarted exit knows nothing
// of the old identity and would otherwise never answer. helloLoop resumes
// the handshake. Caller holds s.mu.
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
	for !s.IsConnected() {
		select {
		case <-s.done:
			return errors.New("session stopped")
		case <-timer.C:
			return errors.New("handshake timed out")
		case <-tick.C:
		}
	}
	return nil
}

// Stop tears down all transports and marks the session stopped.
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

// IsConnected reports whether the session has completed the handshake and
// at least one transport is live.
func (s *Session) IsConnected() bool {
	s.mu.Lock()
	ready := s.ready && !s.stopped
	s.mu.Unlock()
	if !ready {
		return false
	}
	return s.anyLive()
}

// anyLive reports whether at least one transport currently reaches the peer.
func (s *Session) anyLive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.liveLinksLocked()) > 0
}

// liveLocked reports whether a carrier currently reaches the peer. A
// document carrier can be attached to its document (IsConnected) while the
// peer's side of it is down, e.g. stuck on a captcha; once the peer is
// known to answer keepalives, only recently heard carriers count.
// Caller holds s.mu.
func (s *Session) liveLocked(l *transportLink) bool {
	if l == nil || l.dead || !l.started || !l.raw.IsConnected() {
		return false
	}
	return !s.peerKeepalive || time.Since(l.lastHeard) < s.linkTimeout
}

// ActiveTransport names the carrier data currently goes through: the
// highest-priority live one, or "" when none is live.
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

// IsExit reports whether this is the exit node's side of the session.
func (s *Session) IsExit() bool { return s.exit }

// PeerParameters returns the negotiated peer parameters, if ready.
func (s *Session) PeerParameters() (PeerParameters, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.remote, s.ready && !s.stopped
}

// Transports returns the list of transport names currently attached,
// in priority order.
func (s *Session) Transports() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.order...)
}

// Receive installs the IPv4 data callback.
func (s *Session) Receive(cb func([]byte)) {
	s.mu.Lock()
	s.dataCallback = cb
	s.mu.Unlock()
}

// SetControlHandler installs the control-packet callback. Must be called
// before Start. Passing nil disables delivery.
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
	s.mu.Unlock()

	raw, err := env.Encode()
	if err != nil {
		return err
	}
	return link.batched.Send(raw)
}

// ---- IPv4 data ----

// Send routes one complete IPv4 packet through the highest-priority live
// transport (flow-hashed across ties).
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

	// Priority is the failover order: use the best live carrier, and
	// spread flows only across carriers sharing that priority. The flow
	// hash keeps each 4-tuple on one carrier so its ordering is preserved.
	top := links[:1]
	for _, l := range links[1:] {
		if l.priority != links[0].priority {
			break
		}
		top = append(top, l)
	}
	key := extractFlowKeyBytes(p)
	idx := int(flowHashBytes(key) % uint64(len(top)))
	return top[idx].batched.Send(raw)
}

// liveLinksLocked returns the live transports in priority order.
// Caller holds s.mu.
func (s *Session) liveLinksLocked() []*transportLink {
	out := make([]*transportLink, 0, len(s.links))
	for _, name := range s.order {
		if l := s.links[name]; s.liveLocked(l) {
			out = append(out, l)
		}
	}
	return out
}

// SendControl transmits a control packet through the highest-priority live
// transport. Control is not covered by the replay window.
func (s *Session) SendControl(subtype control.Subtype, payload []byte) error {
	s.mu.Lock()
	links := s.liveLinksLocked()
	s.mu.Unlock()
	if len(links) == 0 {
		return errors.New("session: no live transport for control")
	}
	return s.sendControlVia(links[0], subtype, payload)
}

// sendControlVia transmits a control packet through one specific carrier.
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
		return
	}
	if env.Role == s.roleLocked() {
		return
	}
	switch env.Kind {
	case control.KindHello:
		s.receiveHello(link, env)
	case control.KindIPv4:
		s.receiveIPv4(link, p, env)
	case control.KindControl:
		s.receiveControl(link, p, env)
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
	if env.Hello == nil || env.Hello.Reserved != 0 || env.Hello.Ready > 1 {
		s.mu.Unlock()
		return
	}
	sender := env.Local
	if sender == ([32]byte{}) {
		s.mu.Unlock()
		return
	}
	params := PeerParameters{
		Capabilities:  control.Capabilities(env.Hello.Capabilities),
		MaxPacketSize: int(env.Hello.MaxPacketSize),
	}
	if !validParameters(params) {
		s.mu.Unlock()
		return
	}
	if s.ready && sender != s.peer {
		s.offerReplacementLocked(link, sender, params, env)
		return
	}
	if s.ready && (params.Capabilities&s.params.Capabilities != s.remote.Capabilities ||
		minInt(params.MaxPacketSize, s.params.MaxPacketSize) != s.remote.MaxPacketSize) {
		s.mu.Unlock()
		return
	}
	echo := env.Peer == s.local
	zero := env.Peer == ([32]byte{})
	if !echo && !zero {
		s.mu.Unlock()
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
	// Reply through every live transport so the peer sees the new ready
	// state regardless of which one it is listening on.
	var names []string
	if changed || (!wasReady && echo) || (wasReady && env.Hello.Ready == 0) {
		names = append(names, s.order...)
	}
	s.mu.Unlock()

	for _, name := range names {
		_ = s.helloVia(name)
	}
}

// offerReplacementLocked handles a hello from an unknown sender while the
// session is established. Another key holder appearing usually means the
// peer restarted, but it may also be a replay of old traffic seen in the
// carrier (anyone with access to a document sees the ciphertext). So the
// established session is left alone until the sender echoes a challenge
// minted for it just now, which old traffic cannot contain; only then is
// the peer replaced, under that fresh challenge and with a new sequence
// and replay window, so packets of the old session no longer match.
// Called with s.mu held; releases it.
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
		utils.Debugf("[SESSION] peer replaced after a fresh challenge")
		for _, name := range names {
			_ = s.helloVia(name)
		}
		return
	}

	if env.Peer != ([32]byte{}) {
		s.mu.Unlock()
		return
	}
	if cand == nil || cand.sender != sender {
		if now.Sub(s.candidateLast) < candidateInterval {
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
	if env.Data == nil || !s.ready || s.stopped || env.Local != s.peer || env.Peer != s.local {
		s.mu.Unlock()
		return
	}
	payload := p[control.EnvelopeSize:]
	if permittedPacket(payload, s.remote) != nil {
		s.mu.Unlock()
		return
	}
	if !s.acceptSequenceLocked(env.Data.Sequence) {
		s.mu.Unlock()
		return
	}
	link.lastHeard = time.Now()
	cb := s.dataCallback
	s.mu.Unlock()
	if cb != nil {
		cb(append([]byte(nil), payload...))
	}
}

func (s *Session) receiveControl(link *transportLink, p []byte, env *control.Envelope) {
	if env.Control == nil || env.Control.Flags != 0 || env.Control.Subtype == 0 {
		return
	}
	s.mu.Lock()
	if !s.ready || s.stopped || env.Local != s.peer || env.Peer != s.local {
		s.mu.Unlock()
		return
	}
	link.lastHeard = time.Now()
	switch env.Control.Subtype {
	case control.SubtypeLinkPing:
		noPong := s.noPong
		s.mu.Unlock()
		if noPong {
			return
		}
		go func() { _ = s.sendControlVia(link, control.SubtypeLinkPong, nil) }()
		return
	case control.SubtypeLinkPong:
		s.peerKeepalive = true
		s.mu.Unlock()
		return
	}
	cb := s.controlCallback
	s.mu.Unlock()
	if cb == nil {
		return
	}
	payload := append([]byte(nil), p[control.EnvelopeSize:]...)
	go cb(env.Control.Subtype, payload)
}

// acceptSequenceLocked implements the 64-entry sliding replay window.
// Caller holds s.mu.
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

// Stats aggregates counters from all transports.
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

// MarkDead marks a transport as unavailable for routing. Called externally
// when a transport's own error handling decides it cannot recover.
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
