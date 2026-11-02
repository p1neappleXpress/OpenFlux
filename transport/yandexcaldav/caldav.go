// Package yandexcaldav tunnels packets through a private Yandex Calendar
// (caldav.yandex.ru). It is the native twin of the yandex-caldav JS transport:
// the same wire (OFX1 frames in a VEVENT DESCRIPTION), the same two events per
// channel, the same account selection by the aN- channel prefix.
//
// One Transport is one channel. Parallelism is the core's job: a group of
// these lanes is striped by transport.StripedTransport, so this package stays
// a single reliable, ordered carrier.
package yandexcaldav

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/p1neappleXpress/OpenFlux/netbind"
	"github.com/p1neappleXpress/OpenFlux/transport"
	"github.com/p1neappleXpress/OpenFlux/utils"
)

const (
	defaultServer   = "https://caldav.yandex.ru"
	defaultCalendar = "OpenFlux"
	defaultPoll     = 120 * time.Millisecond
	defaultBatch    = 150 * time.Millisecond
	maxEventBytes   = 640 << 10
	maxRingBytes    = 3 << 20
	maxRingFrames   = 4096
	maxPending      = 512
	frameHeader     = 14
	dataPrefix      = "OFX1-"
	userAgent       = "OpenFlux/yandex-caldav"
	caldavNS        = "urn:ietf:params:xml:ns:caldav"
)

var (
	errAuth     = errors.New("yandex-caldav: authentication failed")
	errWaiting  = errors.New("yandex-caldav: calendar is not created yet")
	errFatal    = errors.New("yandex-caldav: fatal")
	errClosed   = errors.New("yandex-caldav: closed")
	accountRe   = regexp.MustCompile(`^a([1-9][0-9]*)-`)
	dataRe      = regexp.MustCompile(`OFX1-([0-9]+)\.([0-9]+)-([A-Za-z0-9+/=]*)`)
	lastModRe   = regexp.MustCompile(`LAST-MODIFIED:(\d{8}T\d{6}Z)`)
	calCreateMu sync.Mutex
)

// ---------------------------------------------------------------------------
// DAV XML
// ---------------------------------------------------------------------------

const (
	pfPrincipal = `<?xml version="1.0" encoding="utf-8"?><d:propfind xmlns:d="DAV:"><d:prop><d:current-user-principal/></d:prop></d:propfind>`
	pfHome      = `<?xml version="1.0" encoding="utf-8"?><d:propfind xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop><c:calendar-home-set/><d:displayname/></d:prop></d:propfind>`
	pfCalendars = `<?xml version="1.0" encoding="utf-8"?><d:propfind xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop><d:resourcetype/><d:displayname/></d:prop></d:propfind>`
	pfETag      = `<?xml version="1.0" encoding="utf-8"?><d:propfind xmlns:d="DAV:"><d:prop><d:getetag/></d:prop></d:propfind>`
)

type multistatus struct {
	XMLName   xml.Name     `xml:"DAV: multistatus"`
	Responses []msResponse `xml:"DAV: response"`
}

type msResponse struct {
	Href      string       `xml:"DAV: href"`
	Propstats []msPropstat `xml:"DAV: propstat"`
}

type msPropstat struct {
	Prop   msProp `xml:"DAV: prop"`
	Status string `xml:"DAV: status"`
}

type hrefValue struct {
	Href string `xml:"DAV: href"`
}

type msProp struct {
	DisplayName          string       `xml:"DAV: displayname"`
	CurrentUserPrincipal hrefValue    `xml:"DAV: current-user-principal"`
	CalendarHomeSet      hrefValue    `xml:"urn:ietf:params:xml:ns:caldav calendar-home-set"`
	ResourceType         resourceType `xml:"DAV: resourcetype"`
	GetETag              string       `xml:"DAV: getetag"`
}

type resourceType struct {
	Types []xml.Name `xml:",any"`
}

func (r resourceType) IsCalendar() bool {
	for _, t := range r.Types {
		if t.Space == caldavNS && t.Local == "calendar" {
			return true
		}
	}
	return false
}

func parseMS(b []byte) (*multistatus, error) {
	var ms multistatus
	if err := xml.Unmarshal(b, &ms); err != nil {
		return nil, err
	}
	return &ms, nil
}

func firstProp(ms *multistatus, pick func(*msProp) string) string {
	for _, r := range ms.Responses {
		for _, ps := range r.Propstats {
			if v := pick(&ps.Prop); v != "" {
				return v
			}
		}
	}
	return ""
}

func firstETag(ms *multistatus) string {
	return firstProp(ms, func(p *msProp) string { return p.GetETag })
}

// ---------------------------------------------------------------------------
// Transport
// ---------------------------------------------------------------------------

type account struct {
	Email    string
	Password string
}

type Transport struct {
	*transport.BaseTransport

	channel      string
	server       string
	email        string
	password     string
	calendarHint string
	accounts     []account
	poll         time.Duration
	batch        time.Duration
	isExit       bool
	client       *http.Client
	epoch        uint32
	startErr     error

	mu   sync.Mutex
	sess *session
}

// New builds one CalDAV carrier. url is the channel (the profile's value);
// params carries email/password/calendar/server/accounts and the timings.
func New(rawURL string, params map[string]interface{}, isExit bool, base transport.TransportConfig) *Transport {
	t := &Transport{
		BaseTransport: transport.NewBaseTransport(base),
		channel:       safeSlug(rawURL),
		server:        normalizeServer(str(params["server"])),
		email:         str(params["email"]),
		password:      str(params["password"]),
		calendarHint:  strings.TrimSpace(str(params["calendar"])),
		accounts:      parseAccounts(str(params["accounts"])),
		poll:          durationParam(params["poll_ms"], 120, 10000, defaultPoll),
		batch:         durationParam(params["batch_ms"], 80, 5000, defaultBatch),
		isExit:        isExit,
		epoch:         uint32(rand.Int31n(0x7ffffffe) + 1),
	}
	if t.channel == "" {
		t.channel = "main"
	}
	if t.calendarHint == "" {
		t.calendarHint = defaultCalendar
	}
	t.applyAccount()
	if t.email == "" {
		t.startErr = fmt.Errorf("yandex-caldav: в настройках не указана почта")
	} else if t.password == "" {
		t.startErr = fmt.Errorf("yandex-caldav: в настройках нет пароля приложения («Календарь»)")
	}
	t.client = newHTTPClient()
	return t
}

func (t *Transport) applyAccount() {
	m := accountRe.FindStringSubmatch(t.channel)
	if m == nil {
		return
	}
	idx, _ := strconv.Atoi(m[1])
	idx--
	if len(t.accounts) == 0 {
		if idx > 0 {
			t.startErr = fmt.Errorf("yandex-caldav: канал %q просит аккаунт a%d, а список аккаунтов пуст", t.channel, idx+1)
		}
		return
	}
	if idx < 0 || idx >= len(t.accounts) {
		t.startErr = fmt.Errorf("yandex-caldav: для канала %q нет аккаунта a%d в списке", t.channel, idx+1)
		return
	}
	t.email = t.accounts[idx].Email
	t.password = t.accounts[idx].Password
}

func (t *Transport) Start() error {
	if t.startErr != nil {
		return t.startErr
	}
	if err := t.BaseTransport.Start(); err != nil {
		return err
	}
	go t.run()
	return nil
}

func (t *Transport) Stop() error {
	t.BaseTransport.Stop()
	t.mu.Lock()
	s := t.sess
	t.sess = nil
	t.mu.Unlock()
	if s != nil {
		s.close()
	}
	return nil
}

func (t *Transport) Send(data []byte) error {
	t.mu.Lock()
	s := t.sess
	t.mu.Unlock()
	if s == nil {
		return errors.New("yandex-caldav: not connected")
	}
	if err := s.enqueue(data); err != nil {
		return err
	}
	t.RecordSend(len(data))
	return nil
}

// PreferredBatchBytes lets the session hand over larger batches: a CalDAV PUT
// carries the whole window anyway.
func (t *Transport) PreferredBatchBytes() int { return 16384 }

func (t *Transport) run() {
	backoff := time.Second
	for t.IsRunning() {
		s, err := t.connect()
		if err != nil {
			t.SetConnected(false)
			if errors.Is(err, errAuth) || errors.Is(err, errFatal) {
				log.Printf("yandex-caldav: %v", err)
				return
			}
			utils.Debugf("[CALDAV] %v", err)
			wait := backoff
			if errors.Is(err, errWaiting) {
				wait = 2 * time.Second
			}
			if !sleepOrDone(t.Done(), wait) {
				return
			}
			if backoff < 15*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		t.mu.Lock()
		t.sess = s
		t.mu.Unlock()
		t.SetConnected(true)
		utils.Debugf("[CALDAV] ready: %s -> %s", s.ownURL, s.peerURL)

		errCh := make(chan error, 2)
		go s.pollLoop(errCh)
		go s.flushLoop(errCh)
		select {
		case <-t.Done():
			s.close()
			return
		case err := <-errCh:
			s.close()
			t.SetConnected(false)
			t.mu.Lock()
			if t.sess == s {
				t.sess = nil
			}
			t.mu.Unlock()
			if errors.Is(err, errAuth) || errors.Is(err, errFatal) {
				log.Printf("yandex-caldav: %v", err)
				return
			}
			utils.Debugf("[CALDAV] session ended: %v", err)
			if !sleepOrDone(t.Done(), backoff) {
				return
			}
		}
	}
}

func (t *Transport) connect() (*session, error) {
	cal, err := t.resolveCalendar()
	if err != nil {
		return nil, err
	}
	s := t.newSession(cal)
	body := s.ownBody()
	resp, err := t.dav("PUT", s.ownURL, body, -1, "text/calendar; charset=utf-8")
	if err != nil {
		return nil, err
	}
	if resp.status < 200 || resp.status >= 300 {
		if resp.status == 401 || resp.status == 403 {
			return nil, errAuth
		}
		if resp.status == 409 {
			return nil, fmt.Errorf("yandex-caldav: calendar disappeared (PUT 409)")
		}
		if resp.status == 413 {
			return nil, fmt.Errorf("%w: event too large (413)", errFatal)
		}
		return nil, fmt.Errorf("yandex-caldav: PUT %s: HTTP %d", s.ownURL, resp.status)
	}
	return s, nil
}

// ---------------------------------------------------------------------------
// Calendar discovery
// ---------------------------------------------------------------------------

func (t *Transport) resolveCalendar() (string, error) {
	if strings.Contains(t.calendarHint, "/") {
		p := t.hrefPath(t.calendarHint)
		if !strings.HasSuffix(p, "/") {
			p += "/"
		}
		resp, err := t.dav("PROPFIND", p, []byte(pfCalendars), 0, "")
		if err != nil {
			return "", err
		}
		if resp.status == 404 {
			return "", fmt.Errorf("%w: календарь не найден по пути %s", errFatal, p)
		}
		if resp.status != 207 {
			return "", fmt.Errorf("%w: PROPFIND %s: HTTP %d", errFatal, p, resp.status)
		}
		return p, nil
	}

	root, err := t.dav("PROPFIND", "/", []byte(pfPrincipal), 0, "")
	if err != nil {
		return "", err
	}
	if root.status != 207 {
		return "", fmt.Errorf("%w: PROPFIND /: HTTP %d", errFatal, root.status)
	}
	ms, err := parseMS(root.body)
	if err != nil {
		return "", err
	}
	principal := t.hrefPath(firstProp(ms, func(p *msProp) string { return p.CurrentUserPrincipal.Href }))
	if principal == "" {
		return "", fmt.Errorf("%w: no current-user-principal", errFatal)
	}

	pr, err := t.dav("PROPFIND", principal, []byte(pfHome), 0, "")
	if err != nil {
		return "", err
	}
	if pr.status != 207 {
		return "", fmt.Errorf("%w: PROPFIND principal: HTTP %d", errFatal, pr.status)
	}
	ms, err = parseMS(pr.body)
	if err != nil {
		return "", err
	}
	home := t.hrefPath(firstProp(ms, func(p *msProp) string { return p.CalendarHomeSet.Href }))
	if home == "" {
		return "", fmt.Errorf("%w: no calendar-home-set", errFatal)
	}
	if !strings.HasSuffix(home, "/") {
		home += "/"
	}

	list, err := t.listCalendars(home)
	if err != nil {
		return "", err
	}
	if c := pickCalendar(list, t.calendarHint); c != "" {
		utils.Debugf("[CALDAV] calendar: %s", c)
		return c, nil
	}
	if !t.isExit {
		return "", errWaiting
	}
	return t.createCalendar(home, t.calendarHint)
}

type calendarEntry struct {
	path string
	name string
}

func (t *Transport) listCalendars(home string) ([]calendarEntry, error) {
	resp, err := t.dav("PROPFIND", home, []byte(pfCalendars), 1, "")
	if err != nil {
		return nil, err
	}
	if resp.status != 207 {
		return nil, fmt.Errorf("PROPFIND %s: HTTP %d", home, resp.status)
	}
	ms, err := parseMS(resp.body)
	if err != nil {
		return nil, err
	}
	var out []calendarEntry
	for _, r := range ms.Responses {
		p := t.hrefPath(r.Href)
		if strings.HasSuffix(p, "/inbox/") || strings.HasSuffix(p, "/outbox/") {
			continue
		}
		cal := false
		name := ""
		for _, ps := range r.Propstats {
			if ps.Prop.ResourceType.IsCalendar() {
				cal = true
			}
			if ps.Prop.DisplayName != "" {
				name = ps.Prop.DisplayName
			}
		}
		if cal {
			out = append(out, calendarEntry{path: p, name: name})
		}
	}
	return out, nil
}

func pickCalendar(list []calendarEntry, want string) string {
	var matches []calendarEntry
	for _, c := range list {
		if strings.EqualFold(c.name, want) {
			matches = append(matches, c)
		}
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].path < matches[j].path })
	if len(matches) > 0 {
		return matches[0].path
	}
	return ""
}

func (t *Transport) createCalendar(home, name string) (string, error) {
	calCreateMu.Lock()
	defer calCreateMu.Unlock()
	if list, err := t.listCalendars(home); err == nil {
		if p := pickCalendar(list, name); p != "" {
			return p, nil
		}
	}
	body := `<?xml version="1.0" encoding="utf-8"?>` +
		`<c:mkcalendar xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">` +
		`<d:set><d:prop><d:displayname>` + xmlEscape(name) + `</d:displayname>` +
		`<c:supported-calendar-component-set><c:comp name="VEVENT"/></c:supported-calendar-component-set>` +
		`</d:prop></d:set></c:mkcalendar>`
	resp, err := t.dav("MKCALENDAR", home+safeSlug(name)+"/", []byte(body), -1, "")
	if err != nil {
		return "", err
	}
	if resp.status != 200 && resp.status != 201 && resp.status != 204 {
		if resp.status == 403 || resp.status == 405 {
			return "", fmt.Errorf("%w: cannot create calendar %q (HTTP %d)", errFatal, name, resp.status)
		}
		return "", fmt.Errorf("MKCALENDAR: HTTP %d", resp.status)
	}
	for i := 0; i < 6; i++ {
		time.Sleep(500 * time.Millisecond)
		list, err := t.listCalendars(home)
		if err != nil {
			continue
		}
		if p := pickCalendar(list, name); p != "" {
			return p, nil
		}
	}
	return "", fmt.Errorf("calendar %q created but not listed", name)
}

// ---------------------------------------------------------------------------
// DAV requests
// ---------------------------------------------------------------------------

type davResponse struct {
	status int
	header http.Header
	body   []byte
}

func (t *Transport) dav(method, target string, body []byte, depth int, ctype string) (*davResponse, error) {
	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		target = t.server + target
	}
	req, err := http.NewRequest(method, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(t.email, t.password)
	req.Header.Set("User-Agent", userAgent)
	if depth >= 0 {
		req.Header.Set("Depth", strconv.Itoa(depth))
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	} else if len(body) > 0 {
		req.Header.Set("Content-Type", `application/xml; charset="utf-8"`)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return &davResponse{status: resp.StatusCode, header: resp.Header, body: data}, errAuth
	}
	return &davResponse{status: resp.StatusCode, header: resp.Header, body: data}, nil
}

func (t *Transport) hrefPath(href string) string {
	h := strings.ReplaceAll(strings.TrimSpace(href), "%40", "@")
	if strings.HasPrefix(h, "http://") || strings.HasPrefix(h, "https://") {
		if u, err := url.Parse(h); err == nil {
			h = u.Path
		}
	}
	if !strings.HasPrefix(h, "/") {
		h = "/" + h
	}
	return h
}

// ---------------------------------------------------------------------------
// One connected channel
// ---------------------------------------------------------------------------

type frame struct {
	epoch uint32
	seq   uint32
	ts    uint32
	data  []byte
}

type session struct {
	t       *Transport
	ownURL  string
	peerURL string
	uid     string
	epoch   uint32

	mu            sync.Mutex
	ring          []frame
	ringBytes     int
	pending       [][]byte
	seq           uint32
	ackEpoch      uint32
	ackSeq        uint32
	lastAcked     uint32
	peerEpoch     int64
	lastSeq       int64
	lastETag      string
	haveETag      bool
	firstSnapshot bool
	dirty         bool
	closed        bool
	stop          chan struct{}
	wake          chan struct{}
}

func (t *Transport) newSession(cal string) *session {
	own := "ofx-" + t.channel
	if t.isExit {
		own += "-s2c"
	} else {
		own += "-c2s"
	}
	peer := "ofx-" + t.channel
	if t.isExit {
		peer += "-c2s"
	} else {
		peer += "-s2c"
	}
	return &session{
		t:         t,
		ownURL:    cal + own + ".ics",
		peerURL:   cal + peer + ".ics",
		uid:       own,
		epoch:     t.epoch,
		peerEpoch: -1,
		lastSeq:   -1,
		stop:      make(chan struct{}),
		wake:      make(chan struct{}, 1),
	}
}

func (s *session) close() {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.stop)
	}
	s.mu.Unlock()
}

func (s *session) enqueue(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errClosed
	}
	if len(s.pending) >= maxPending {
		return errors.New("yandex-caldav: send queue is full")
	}
	cp := append([]byte(nil), data...)
	s.pending = append(s.pending, cp)
	s.dirty = true
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return nil
}

func (s *session) flushLoop(errCh chan<- error) {
	ticker := time.NewTicker(s.t.batch)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			if err := s.flush(); err != nil {
				errCh <- err
				return
			}
		case <-s.wake:
			if err := s.flush(); err != nil {
				errCh <- err
				return
			}
		}
	}
}

func (s *session) flush() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	now := time.Now()
	for _, p := range s.pending {
		s.seq++
		s.ring = append(s.ring, frame{epoch: s.epoch, seq: s.seq, ts: uint32(now.Unix()), data: p})
		s.ringBytes += frameHeader + len(p)
	}
	s.pending = nil
	s.trimLocked(now)
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	body := s.ownBodyLocked()
	s.dirty = false
	s.mu.Unlock()

	resp, err := s.t.dav("PUT", s.ownURL, body, -1, "text/calendar; charset=utf-8")
	if err != nil {
		s.mu.Lock()
		s.dirty = true
		s.mu.Unlock()
		utils.Debugf("[CALDAV] PUT: %v", err)
		return nil
	}
	switch {
	case resp.status >= 200 && resp.status < 300:
		return nil
	case resp.status == 401 || resp.status == 403:
		return errAuth
	case resp.status == 409:
		return fmt.Errorf("calendar disappeared (PUT 409)")
	case resp.status == 413:
		return fmt.Errorf("%w: event too large (413)", errFatal)
	default:
		s.mu.Lock()
		s.dirty = true
		s.mu.Unlock()
		utils.Debugf("[CALDAV] PUT %s: HTTP %d", s.ownURL, resp.status)
		return nil
	}
}

func (s *session) pollLoop(errCh chan<- error) {
	ticker := time.NewTicker(s.t.poll)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			if err := s.pollOnce(); err != nil {
				errCh <- err
				return
			}
		}
	}
}

func (s *session) pollOnce() error {
	resp, err := s.t.dav("PROPFIND", s.peerURL, []byte(pfETag), 0, "")
	if err != nil {
		if errors.Is(err, errAuth) {
			return err
		}
		utils.Debugf("[CALDAV] PROPFIND peer: %v", err)
		return nil
	}
	if resp.status == 404 {
		s.mu.Lock()
		s.haveETag = false
		s.mu.Unlock()
		return nil
	}
	if resp.status != 207 {
		utils.Debugf("[CALDAV] PROPFIND peer: HTTP %d", resp.status)
		return nil
	}
	ms, err := parseMS(resp.body)
	if err != nil {
		return nil
	}
	etag := firstETag(ms)
	s.mu.Lock()
	changed := !s.haveETag || etag != s.lastETag
	s.mu.Unlock()
	if !changed {
		return nil
	}

	got, err := s.t.dav("GET", s.peerURL, nil, -1, "")
	if err != nil {
		if errors.Is(err, errAuth) {
			return err
		}
		return nil
	}
	if got.status != 200 {
		return nil
	}
	s.mu.Lock()
	first := !s.firstSnapshot
	s.firstSnapshot = true
	s.lastETag = etag
	s.haveETag = true
	s.mu.Unlock()

	emit := true
	if first {
		if age := serverAge(got.header.Get("Date"), got.body); age > 10*time.Second {
			emit = false
			utils.Debugf("[CALDAV] ignoring peer event from %v ago", age.Round(time.Second))
		}
	}
	payloads, ack := s.ingest(got.body, emit)
	for _, p := range payloads {
		s.t.RecordReceive(len(p))
		s.t.CallReceive(p)
	}
	if ack {
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
	return nil
}

func (s *session) ingest(body []byte, emit bool) ([][]byte, bool) {
	flat := unfold(string(body))
	m := dataRe.FindStringSubmatch(flat)
	if m == nil {
		return nil, false
	}
	peerAckEpoch, _ := strconv.ParseUint(m[1], 10, 32)
	peerAckSeq, _ := strconv.ParseUint(m[2], 10, 32)
	raw, err := base64.StdEncoding.DecodeString(m[3])
	if err != nil {
		return nil, false
	}
	frames, err := decodeFrames(raw)
	if err != nil {
		return nil, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if uint32(peerAckEpoch) == s.epoch && uint32(peerAckSeq) > s.lastAcked {
		s.lastAcked = uint32(peerAckSeq)
		s.trimLocked(time.Now())
	}
	var out [][]byte
	ack := false
	for _, f := range frames {
		if int64(f.epoch) != s.peerEpoch {
			s.peerEpoch = int64(f.epoch)
			s.lastSeq = -1
		}
		if int64(f.seq) <= s.lastSeq {
			continue
		}
		s.lastSeq = int64(f.seq)
		ack = true
		if emit {
			out = append(out, f.data)
		}
	}
	if ack {
		s.ackEpoch = uint32(s.peerEpoch)
		s.ackSeq = uint32(s.lastSeq)
		s.dirty = true
	}
	return out, ack
}

func (s *session) trimLocked(now time.Time) {
	cutoff := now.Unix() - 15
	for len(s.ring) > 1 && int64(s.ring[0].ts) < cutoff {
		s.ringBytes -= frameHeader + len(s.ring[0].data)
		s.ring = s.ring[1:]
	}
	for len(s.ring) > maxRingFrames {
		s.ringBytes -= frameHeader + len(s.ring[0].data)
		s.ring = s.ring[1:]
	}
	for len(s.ring) > 1 && s.ringBytes > maxRingBytes {
		s.ringBytes -= frameHeader + len(s.ring[0].data)
		s.ring = s.ring[1:]
	}
	for len(s.ring) > 0 && s.ring[0].epoch == s.epoch && s.ring[0].seq <= s.lastAcked {
		s.ringBytes -= frameHeader + len(s.ring[0].data)
		s.ring = s.ring[1:]
	}
}

func (s *session) windowLocked() []frame {
	var out []frame
	total := 0
	for _, f := range s.ring {
		size := frameHeader + len(f.data)
		if len(out) > 0 && total+size > maxEventBytes {
			break
		}
		out = append(out, f)
		total += size
		if total >= maxEventBytes {
			break
		}
	}
	return out
}

func (s *session) ownBody() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ownBodyLocked()
}

func (s *session) ownBodyLocked() []byte {
	return buildICS(s.uid, s.ackEpoch, s.ackSeq, s.windowLocked())
}

// ---------------------------------------------------------------------------
// Wire
// ---------------------------------------------------------------------------

func buildICS(uid string, ackE, ackS uint32, frames []frame) []byte {
	b64 := ""
	if len(frames) > 0 {
		b64 = base64.StdEncoding.EncodeToString(encodeFrames(frames))
	}
	line := "DESCRIPTION:" + dataPrefix + strconv.FormatUint(uint64(ackE), 10) + "." +
		strconv.FormatUint(uint64(ackS), 10) + "-" + b64
	return []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//OpenFlux//yandex-caldav//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:" + uid + "\r\nDTSTAMP:" + time.Now().UTC().Format("20060102T150405Z") + "\r\n" +
		"DTSTART:20300101T000000Z\r\nDTEND:20300101T000100Z\r\nSUMMARY:OpenFlux\r\n" +
		foldLine(line) + "\r\nTRANSP:TRANSPARENT\r\nCLASS:PRIVATE\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
}

func encodeFrames(frames []frame) []byte {
	total := 0
	for _, f := range frames {
		total += frameHeader + len(f.data)
	}
	out := make([]byte, total)
	off := 0
	for _, f := range frames {
		binary.BigEndian.PutUint32(out[off:], f.epoch)
		binary.BigEndian.PutUint32(out[off+4:], f.seq)
		binary.BigEndian.PutUint32(out[off+8:], f.ts)
		binary.BigEndian.PutUint16(out[off+12:], uint16(len(f.data)))
		copy(out[off+frameHeader:], f.data)
		off += frameHeader + len(f.data)
	}
	return out
}

func decodeFrames(b []byte) ([]frame, error) {
	var out []frame
	for off := 0; off+frameHeader <= len(b); {
		f := frame{
			epoch: binary.BigEndian.Uint32(b[off:]),
			seq:   binary.BigEndian.Uint32(b[off+4:]),
			ts:    binary.BigEndian.Uint32(b[off+8:]),
		}
		n := int(binary.BigEndian.Uint16(b[off+12:]))
		off += frameHeader
		if n > len(b)-off {
			return nil, errors.New("truncated frame")
		}
		f.data = append([]byte(nil), b[off:off+n]...)
		off += n
		out = append(out, f)
	}
	return out, nil
}

func unfold(s string) string {
	s = strings.ReplaceAll(s, "\r\n ", "")
	return strings.ReplaceAll(s, "\n ", "")
}

func foldLine(line string) string {
	if len(line) <= 73 {
		return line
	}
	var b strings.Builder
	b.WriteString(line[:73])
	for i := 73; i < len(line); i += 72 {
		end := i + 72
		if end > len(line) {
			end = len(line)
		}
		b.WriteString("\r\n ")
		b.WriteString(line[i:end])
	}
	return b.String()
}

func serverAge(dateHeader string, body []byte) time.Duration {
	if dateHeader == "" {
		return 0
	}
	server, err := http.ParseTime(dateHeader)
	if err != nil {
		return 0
	}
	m := lastModRe.FindStringSubmatch(unfold(string(body)))
	if m == nil {
		return 0
	}
	lm, err := time.Parse("20060102T150405Z", m[1])
	if err != nil {
		return 0
	}
	return server.Sub(lm)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func newHTTPClient() *http.Client {
	d := netbind.Wrap(&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second})
	tr := &http.Transport{
		DialContext:         d.DialContext,
		MaxIdleConns:        16,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
	}
	return &http.Client{
		Transport: tr,
		Timeout:   30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func normalizeServer(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		s = defaultServer
	}
	if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return defaultServer
	}
	return u.Scheme + "://" + u.Host
}

func str(v interface{}) string {
	if v == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(v))
}

func durationParam(v interface{}, minMs, maxMs int, def time.Duration) time.Duration {
	s := str(v)
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return def
	}
	if n < minMs {
		n = minMs
	}
	if n > maxMs {
		n = maxMs
	}
	return time.Duration(n) * time.Millisecond
}

func parseAccounts(raw string) []account {
	var out []account
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == '|' || r == '\n' }) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		i := strings.IndexByte(part, ':')
		if i <= 0 {
			continue
		}
		out = append(out, account{Email: strings.TrimSpace(part[:i]), Password: strings.TrimSpace(part[i+1:])})
	}
	return out
}

func safeSlug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if len(out) > 48 {
		out = out[:48]
	}
	return out
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

func sleepOrDone(done <-chan struct{}, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-done:
		return false
	case <-t.C:
		return true
	}
}
