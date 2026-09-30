package transport

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport/control"
	"github.com/p1neappleXpress/OpenFlux/utils"
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
	window           replayWindow
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

	// Classic fallback, see SetClassic.
	classic      ClassicMode
	classicCodec string
	classicLink  *transportLink // exit: the carrier a classic client was last heard on
	classicSeen  bool           // client: the exit answered in classic mode
	classicSince time.Time      // client: when data started going out classic
	alternates   []string       // KDF contexts the peer may derive instead, see KDFContexts

	cntClassicSent atomic.Uint64
	cntClassicRecv atomic.Uint64
	cntClassicDrop atomic.Uint64
}

// ClassicMode is how a Session treats peers of the classic (pre-Session)
// layering.
type ClassicMode int

const (
	// ClassicOff: Session peers only (--negotiate, and every exit that was
	// configured as a Session). Classic frames are dropped with a log line.
	ClassicOff ClassicMode = iota
	// ClassicFallback (client, one carrier): until the exit answers the
	// Session handshake, IPv4 goes out in the classic layering, so an exit
	// that predates Session, or runs classic, still works; the client keeps
	// offering the handshake and switches to the Session once answered.
	ClassicFallback
	// ClassicAccept (exit configured classic): the exit also serves
	// classic clients, as long as no Session client is active. Session
	// clients get a Session, so an updated client never runs classic.
	ClassicAccept
)

// classicWait is how long a client with classic fallback waits for the
// Session handshake before letting data go out classic.
const classicWait = 3 * time.Second

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
	demux     *frameDemux
	encrypted *EncryptedTransport
	batched   *BatchedTransport
	priority  int

	// The classic pipeline on the same carrier, nil unless SetClassic:
	// classicEnc(classicCodec(classic side of demux)).
	classicEnc   *EncryptedTransport
	classicCodec *CodecTransport
	classicHeard time.Time

	started   bool
	lastHeard time.Time
	dead      bool
}

// SetClassic lets this Session also speak the classic layering: a client
// falls back to it while the exit does not answer the handshake
// (ClassicFallback), an exit serves classic clients (ClassicAccept). codec is
// the preferred classic framing (CodecBatched or CodecLegacy); the other one
// is accepted too and tried when the peer stays silent. Call before
// AddTransport.
func (s *Session) SetClassic(codec string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exit {
		s.classic = ClassicAccept
	} else {
		s.classic = ClassicFallback
	}
	s.classicCodec = codec
	utils.Debugf("[SESSION] classic mode: %s (preferred codec %s)", s.classic, codecName(codec != CodecLegacy))
}

func (m ClassicMode) String() string {
	switch m {
	case ClassicFallback:
		return "fallback"
	case ClassicAccept:
		return "accept"
	}
	return "off"
}

// SetAlternateContexts gives every carrier, present and future, the KDF
// contexts the peer may derive its keys from instead of this side's (see
// KDFContexts and EncryptedTransport).
func (s *Session) SetAlternateContexts(contexts []string) {
	s.mu.Lock()
	s.alternates = append(s.alternates, contexts...)
	links := make([]*transportLink, 0, len(s.links))
	for _, l := range s.links {
		links = append(links, l)
	}
	s.mu.Unlock()
	for _, l := range links {
		l.encrypted.SetAlternateContexts(contexts)
	}
}

// newLink builds a carrier's pipelines: the Session's
// batched(encrypted(session side)) and, with classic mode on, the classic
// encrypted(codec(classic side)) sharing its keys.
func (s *Session) newLink(name string, raw Transport, secret, context string, priority int) (*transportLink, error) {
	d := newFrameDemux(raw, name)
	enc, err := NewEncryptedTransport(d.side(true), secret, context, s.exit)
	if err != nil {
		return nil, fmt.Errorf("session: wrap %q: %w", name, err)
	}
	s.mu.Lock()
	alternates := append([]string(nil), s.alternates...)
	classic, codec := s.classic, s.classicCodec
	s.mu.Unlock()
	enc.SetAlternateContexts(alternates)
	link := &transportLink{
		name:      name,
		raw:       raw,
		demux:     d,
		encrypted: enc,
		batched:   NewBatchedTransport(enc),
		priority:  priority,
	}
	if classic != ClassicOff {
		link.classicCodec = NewCodecTransport(d.side(false), codec, !s.exit)
		link.classicEnc = enc.SharingKeys(link.classicCodec)
	}
	return link, nil
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

	link, err := s.newLink(name, raw, secret, context, priority)
	if err != nil {
		return err
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
	if s.classic == ClassicFallback && len(links) != 1 {
		// A classic exit has exactly one carrier; several can only mean a
		// Session exit.
		utils.Debugf("[SESSION] classic fallback off: %d carriers (a classic exit has one)", len(links))
		s.classic = ClassicOff
		for _, l := range links {
			l.classicEnc, l.classicCodec = nil, nil
		}
	}
	classic := s.classic
	s.mu.Unlock()

	role := "client"
	if exit {
		role = "exit"
	}
	utils.Debugf("[SESSION] Start role=%s timeout=%v local=%s transports=%v classic=%s",
		role, timeout, shortID(local), order, classic)

	for _, link := range links {
		if err := s.startLink(link); err != nil {
			utils.Infof("[SESSION] carrier %q failed to start: %v; retrying in the background", link.name, err)
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

	if classic == ClassicFallback {
		wait := min(classicWait, timeout)
		if err := s.waitReady(wait); err == nil {
			s.logHandshake()
			return nil
		}
		s.mu.Lock()
		stopped := s.stopped
		if !stopped && !s.ready {
			s.classicSince = time.Now()
		}
		s.mu.Unlock()
		if stopped {
			return errors.New("session: stopped")
		}
		utils.Infof("[SESSION] no Session handshake from the exit within %v on %q: sending in classic mode meanwhile; "+
			"switching to the Session as soon as the exit answers", wait, order[0])
		return nil
	}

	utils.Debugf("[SESSION] client: waiting for handshake (timeout=%v)", timeout)
	if err := s.waitReady(timeout); err != nil {
		utils.Debugf("[SESSION] handshake FAILED after %v: %v", timeout, err)
		s.dumpDiagnostics("handshake-failure")
		_ = s.Stop()
		return fmt.Errorf("session: handshake failed: %w", err)
	}

	s.logHandshake()
	return nil
}

func (s *Session) logHandshake() {
	s.mu.Lock()
	peer := s.peer
	remote := s.remote
	s.mu.Unlock()
	utils.Debugf("[SESSION] handshake OK: peer=%s caps=0x%x maxPacket=%d",
		shortID(peer), remote.Capabilities, remote.MaxPacketSize)
	s.dumpDiagnostics("handshake-success")
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
			utils.Debugf("[SESSION-DIAG]   crypto link=%q sendOK=%d sendErr=%d recvOK=%d recvFail=%d recvReplay=%d badHdr=%d badLen=%d ctx=%q",
				name, so, se, ro, rf, rr, bh, bl, l.encrypted.Context())
		}
		if l.classicCodec != nil {
			utils.Debugf("[SESSION-DIAG]   classic link=%q codec=%s heard=%v", name, l.classicCodec.Current(), !l.classicHeard.IsZero())
		}
	}
	utils.Debugf("[SESSION-DIAG] classic mode=%s sent=%d recv=%d drop=%d", s.classic, s.cntClassicSent.Load(), s.cntClassicRecv.Load(), s.cntClassicDrop.Load())
	s.mu.Unlock()
	if why != "handshake-success" {
		utils.Infof("[SESSION] handshake not complete (%s): %s", why, s.diagnose())
	}
}

// diagnose names the likeliest reason the exit has not answered the
// handshake, from what did arrive on the carriers.
func (s *Session) diagnose() string {
	s.mu.Lock()
	var recv, ok, fail uint64
	connected := false
	for _, l := range s.links {
		recv += l.raw.Stats().PacketsRecv
		connected = connected || l.raw.IsConnected()
		if l.encrypted != nil {
			_, _, ro, rf, _, _, _ := l.encrypted.CryptoStats()
			ok += ro
			fail += rf
		}
	}
	classicHeard := s.classicSeen
	s.mu.Unlock()
	hellos := s.cntHelloRecv.Load()
	switch {
	case !connected:
		return "no carrier is connected on this side (document unreachable, captcha, or network)"
	case classicHeard:
		return "the exit answers in classic mode only (an old or classic exit); traffic flows classic"
	case recv == 0:
		return "nothing arrived from the exit on any carrier: it is not running, uses another document/room, or is on another carrier"
	case ok == 0 && fail > 0:
		return "packets arrived but none decrypted under any KDF context: the encryption key differs from the exit's"
	case ok > 0 && hellos == 0:
		return "packets decrypted but none was a Session hello: the exit runs classic mode"
	}
	return fmt.Sprintf("%d frames arrived, %d decrypted, %d hellos; see [SESSION-DIAG] at -dd", recv, ok, hellos)
}

func (s *Session) startLink(link *transportLink) error {
	if err := link.batched.Start(); err != nil {
		return err
	}
	link.batched.Receive(func(p []byte) { s.receive(link, p) })
	if link.classicCodec != nil {
		if err := link.classicCodec.Start(); err != nil {
			utils.Debugf("[SESSION] %q classic pipeline: %v", link.name, err)
		} else {
			link.classicEnc.Receive(func(p []byte) { s.receiveClassic(link, p) })
		}
	}

	s.mu.Lock()
	stopped := s.stopped
	ready := s.ready
	if !stopped {
		link.started = true
	}
	s.mu.Unlock()
	if stopped {
		link.stop()
		return errors.New("session stopped")
	}
	// A carrier that comes up in an established session (e.g. once a check
	// was passed) is probed at once: the pong marks it heard, instead of it
	// waiting for the next keepalive tick.
	if ready {
		go func() { _ = s.sendControlVia(link, control.SubtypeLinkPing, nil) }()
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
				utils.Infof("[SESSION] carrier %q up after %d retries", link.name, attempt)
				return
			}
			utils.Debugf("[SESSION] transport %q start retry #%d: %v", link.name, attempt, err)
			if utils.Throttled("session.restart."+link.name, 2*time.Minute) {
				utils.Infof("[SESSION] carrier %q still not up after %d retries: %v", link.name, attempt, err)
			}
			if delay *= 2; delay > s.restartMax {
				delay = s.restartMax
			}
		}
	}()
}

// helloBackoff is when the hello pace drops from helloInterval to
// helloSlow; an exit proven classic gets one every helloClassic.
const (
	helloBackoff = 20 * time.Second
	helloSlow    = 2 * time.Second
	helloClassic = 10 * time.Second
)

func (s *Session) helloLoop() {
	defer s.wg.Done()
	tick := time.NewTicker(s.helloInterval)
	defer tick.Stop()
	attempt := 0
	started := time.Now()
	var lastSent time.Time
	for {
		s.mu.Lock()
		ready := s.ready
		pace := s.helloInterval
		if time.Since(started) > helloBackoff {
			pace = max(pace, helloSlow)
		}
		if s.classicSeen {
			pace = max(pace, helloClassic)
		}
		var names []string
		if !ready && time.Since(lastSent) >= pace {
			for _, name := range s.order {
				if s.links[name].started {
					names = append(names, name)
				}
			}
		}
		if ready {
			started = time.Now()
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
		if len(names) > 0 {
			lastSent = time.Now()
		}
		for _, name := range names {
			if err := s.helloVia(name); err != nil {
				utils.Debugf("[SESSION] hello via %q: %v", name, err)
			}
		}
		if attempt == 80 && !ready {
			attempt++
			s.dumpDiagnostics("handshake-slow")
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
			utils.Infof("[SESSION] exit silent on every carrier for %v: handshaking again", s.linkTimeout)
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
	s.sequence, s.highest, s.window = 0, 0, replayWindow{}
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
	for !s.handshakeDone() {
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
			l.stop()
		}
	})
	s.wg.Wait()
	return nil
}

// stop takes down the carrier once: the batched wrapper stops it through
// the encryption layer and the demux, the classic pipeline only its own
// queue.
func (l *transportLink) stop() {
	if l.classicCodec != nil {
		_ = l.classicCodec.Stop()
	}
	_ = l.batched.Stop()
}

func (s *Session) IsConnected() bool {
	s.mu.Lock()
	ready := s.ready && !s.stopped
	classic := s.classicConnectedLocked()
	s.mu.Unlock()
	if classic {
		return true
	}
	if !ready {
		return false
	}
	return s.anyLive()
}

// handshakeDone reports whether the Session handshake completed and a
// carrier reaches the peer (IsConnected also counts classic mode).
func (s *Session) handshakeDone() bool {
	s.mu.Lock()
	ready := s.ready && !s.stopped
	s.mu.Unlock()
	return ready && s.anyLive()
}

// classicConnectedLocked: a classic-fallback client counts as connected
// while its carrier is (what classic mode always reported); an exit while
// a classic client was heard lately. Caller holds s.mu.
func (s *Session) classicConnectedLocked() bool {
	if s.stopped || s.ready {
		return false
	}
	switch s.classic {
	case ClassicFallback:
		l := s.classicTargetLocked()
		return l != nil && s.connectedLocked(l)
	case ClassicAccept:
		l := s.classicLink
		return l != nil && time.Since(l.classicHeard) < s.linkTimeout
	}
	return false
}

// classicTargetLocked is the carrier classic IPv4 goes out on, or nil.
// Caller holds s.mu.
func (s *Session) classicTargetLocked() *transportLink {
	switch s.classic {
	case ClassicFallback:
		if len(s.order) == 1 {
			if l := s.links[s.order[0]]; l.started && l.classicEnc != nil {
				return l
			}
		}
	case ClassicAccept:
		return s.classicLink
	}
	return nil
}

// sessionActiveLocked reports whether the Session client was heard
// recently enough that a classic client must not take the exit's replies
// from it: a live client sends a keepalive at least every
// keepaliveInterval, so half that again covers one lost. A client that
// went away yields to a classic one after that, not after linkTimeout.
// Caller holds s.mu.
func (s *Session) sessionActiveLocked() bool {
	if !s.ready {
		return false
	}
	window := s.keepaliveInterval * 3 / 2
	for _, l := range s.links {
		if s.connectedLocked(l) && time.Since(l.lastHeard) < window {
			return true
		}
	}
	return false
}

// Mode names what IPv4 currently goes out as: "session", "classic" or ""
// (nothing yet).
func (s *Session) Mode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.stopped:
		return ""
	case s.exit && s.classicLink != nil && !s.sessionActiveLocked():
		return "classic"
	case s.ready:
		return "session"
	case s.classicTargetLocked() != nil:
		return "classic"
	}
	return ""
}

func (s *Session) anyLive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.liveLinksLocked()) > 0
}

// connectedLocked reports whether a carrier is up on this side.
// Caller holds s.mu.
func (s *Session) connectedLocked(l *transportLink) bool {
	return l != nil && !l.dead && l.started && l.raw.IsConnected()
}

// heardLocked reports whether the peer was heard on a carrier within
// linkTimeout: the carrier is known to work both ways, not just to be
// attached to its document on this side. Caller holds s.mu.
func (s *Session) heardLocked(l *transportLink) bool {
	return s.connectedLocked(l) && time.Since(l.lastHeard) < s.linkTimeout
}

func (s *Session) ActiveTransport() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.classicConnectedLocked() {
		if l := s.classicTargetLocked(); l != nil {
			return l.name
		}
	}
	if !s.ready || s.stopped {
		return ""
	}
	if links := s.liveLinksLocked(); len(links) > 0 {
		return links[0].name
	}
	return ""
}

// ActiveTransports names every carrier data goes through now: the
// highest-priority live ones, which Send spreads flows over. ActiveTransport
// is the first of them. Empty before the handshake or after Stop.
func (s *Session) ActiveTransports() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready || s.stopped {
		return nil
	}
	var names []string
	for _, l := range topLinks(s.liveLinksLocked()) {
		names = append(names, l.name)
	}
	return names
}

// topLinks is the group of live links Send uses: the first one and every
// following one of the same priority.
func topLinks(links []*transportLink) []*transportLink {
	if len(links) == 0 {
		return nil
	}
	top := links[:1:1]
	for _, l := range links[1:] {
		if l.priority != links[0].priority {
			break
		}
		top = append(top, l)
	}
	return top
}

// LiveTransports names the carriers that currently reach the peer, highest
// priority first. Empty before the handshake or after Stop.
func (s *Session) LiveTransports() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.classicConnectedLocked() {
		if l := s.classicTargetLocked(); l != nil {
			return []string{l.name}
		}
	}
	if !s.ready || s.stopped {
		return nil
	}
	var names []string
	for _, l := range s.liveLinksLocked() {
		names = append(names, l.name)
	}
	return names
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
	if !s.stopped && s.classic != ClassicOff {
		// A client whose exit has not answered the handshake, or an exit
		// whose current client is a classic one: classic layering.
		if l := s.classicTargetLocked(); l != nil && (!s.ready || (s.exit && !s.sessionActiveLocked())) {
			s.mu.Unlock()
			return s.sendClassic(l, p)
		}
	}
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

	top := topLinks(links)
	key := extractFlowKeyBytes(p)
	idx := int(flowHashBytes(key) % uint64(len(top)))
	chosen := top[idx]

	n := s.cntDataSent.Add(1)
	if n == 1 || n%100 == 0 {
		utils.Debugf("[SESSION] send IPv4 #%d seq=%d via %q size=%d proto=%d", n, seq, chosen.name, len(p), p[9])
	}
	return chosen.batched.Send(raw)
}

// liveLinksLocked returns the carriers to route through, in priority order.
//
// Carriers the peer has been heard on come first; a carrier that is merely
// connected on this side may be stuck on the other (a document attached
// while the peer's side waits on a captcha), and ranking it by priority
// alone sends everything into it until keepalives catch up. Only when no
// carrier has been heard lately (a peer that predates keepalive and is
// idle, or the heard ones just went away) do connected carriers stand in,
// so sending is still tried rather than refused.
// Caller holds s.mu.
func (s *Session) liveLinksLocked() []*transportLink {
	out := make([]*transportLink, 0, len(s.links))
	for _, name := range s.order {
		if l := s.links[name]; s.heardLocked(l) {
			out = append(out, l)
		}
	}
	if len(out) > 0 {
		return out
	}
	for _, name := range s.order {
		if l := s.links[name]; s.connectedLocked(l) {
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

// ---- classic layering ----

// sendClassic sends one IPv4 packet in the classic layering on link.
func (s *Session) sendClassic(link *transportLink, p []byte) error {
	if len(p) < 20 || p[0]>>4 != 4 {
		return errors.New("session: classic mode carries IPv4 packets only")
	}
	n := s.cntClassicSent.Add(1)
	if n == 1 || n%500 == 0 {
		utils.Debugf("[SESSION] classic send #%d via %q size=%d codec=%s ctx=%s",
			n, link.name, len(p), link.classicCodec.Current(), utils.Sha256Short([]byte(link.classicEnc.Context())))
	}
	return link.classicEnc.Send(p)
}

// receiveClassic takes one decrypted IPv4 packet of the classic layering.
func (s *Session) receiveClassic(link *transportLink, p []byte) {
	if len(p) < 20 || p[0]>>4 != 4 {
		s.cntClassicDrop.Add(1)
		hint := ""
		if len(p) > 0 && p[0] == 0xFF {
			hint = " (a control frame of the iOS fork's own protocol, which this core does not speak; update the app to one built on this core)"
		}
		utils.Debugf("[SESSION] classic frame from %q is not IPv4 (%d bytes)%s", link.name, len(p), hint)
		return
	}
	now := time.Now()
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	if s.exit {
		if s.sessionActiveLocked() {
			s.cntClassicDrop.Add(1)
			s.mu.Unlock()
			if utils.Throttled("session.classic.busy", 30*time.Second) {
				utils.Infof("[SESSION] classic packets on %q ignored: a Session client is active (one client per exit; another device uses the same key?)", link.name)
			}
			return
		}
		if s.classicLink != link {
			utils.Infof("[SESSION] classic client on %q (it does not speak the Session handshake): serving it in classic mode; update the client to get a Session", link.name)
		}
		s.classicLink = link
	} else if !s.classicSeen {
		s.classicSeen = true
		utils.Infof("[SESSION] the exit answers in classic mode on %q (it predates Session or runs classic): staying classic, still offering the handshake", link.name)
	}
	link.classicHeard = now
	cb := s.dataCallback
	s.mu.Unlock()
	n := s.cntClassicRecv.Add(1)
	if n == 1 || n%500 == 0 {
		utils.Debugf("[SESSION] classic recv #%d from %q size=%d", n, link.name, len(p))
	}
	if cb != nil {
		cb(append([]byte(nil), p...))
	}
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
	if echo && !wasReady && !s.exit && (s.classicSeen || !s.classicSince.IsZero()) {
		utils.Infof("[SESSION] the exit answered the Session handshake on %q: switching from classic to the Session", link.name)
	}
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
		s.sequence, s.highest, s.window = 0, 0, replayWindow{}
		s.peerKeepalive = false
		s.candidate = nil
		for _, l := range s.links {
			l.lastHeard = time.Time{}
		}
		link.lastHeard = now
		names := append([]string(nil), s.order...)
		s.mu.Unlock()
		utils.Debugf("[SESSION] peer REPLACED by %s after fresh challenge echo", shortID(sender))
		if s.exit {
			utils.Infof("[SESSION] a new client took over the session on %q: the previous one restarted, or two devices use the same key (the exit serves one client at a time)", link.name)
		}
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

// replayWindowSize is how far behind the newest sequence a data packet may
// arrive and still be accepted once. Carriers of equal priority share
// flows and can differ in latency by hundreds of milliseconds, thousands
// of packets at speed, and cupsonline reorders whole batches across its
// rooms; a 64-packet window dropped nearly all a slower carrier delivered.
const replayWindowSize = 4096

// replayWindow is a ring of bits, one per sequence number within
// replayWindowSize of the newest one: set once that sequence was accepted.
type replayWindow [replayWindowSize / 64]uint64

func (w *replayWindow) has(seq uint64) bool {
	i := seq % replayWindowSize
	return w[i/64]&(1<<(i%64)) != 0
}

func (w *replayWindow) set(seq uint64) {
	i := seq % replayWindowSize
	w[i/64] |= 1 << (i % 64)
}

func (w *replayWindow) clear(seq uint64) {
	i := seq % replayWindowSize
	w[i/64] &^= 1 << (i % 64)
}

// acceptSequenceLocked accepts each sequence number at most once, and only
// within replayWindowSize of the newest one seen. Caller holds s.mu.
func (s *Session) acceptSequenceLocked(seq uint64) bool {
	if seq == 0 {
		return false
	}
	if seq > s.highest {
		// The slots of the numbers skipped over still hold bits from a
		// full turn of the ring ago.
		if seq-s.highest >= replayWindowSize {
			s.window = replayWindow{}
		} else {
			for n := s.highest + 1; n < seq; n++ {
				s.window.clear(n)
			}
		}
		s.highest = seq
		s.window.clear(seq)
		s.window.set(seq)
		return true
	}
	if s.highest-seq >= replayWindowSize || s.window.has(seq) {
		return false
	}
	s.window.set(seq)
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

// MarkStalled forgets that the peer was heard on a carrier, so routing
// prefers the others until something arrives on it again. It is for a
// carrier known to be stuck, e.g. one that reported a captcha: it may stay
// connected while nothing gets through.
func (s *Session) MarkStalled(name string) {
	s.mu.Lock()
	if l, ok := s.links[name]; ok && !l.lastHeard.IsZero() {
		l.lastHeard = time.Time{}
		utils.Debugf("[SESSION] %q stalled: routing around it until the peer is heard on it", name)
	}
	s.mu.Unlock()
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
