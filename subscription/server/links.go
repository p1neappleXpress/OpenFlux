package server

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/p1neappleXpress/OpenFlux/provision"
	"github.com/p1neappleXpress/OpenFlux/share"
	"github.com/p1neappleXpress/OpenFlux/subscription"
)

// channelInfo is a node channel as node-install.sh left it on disk.
type channelInfo struct {
	key        string
	port       int
	context    string
	transports []provision.ChannelTransport
}

// readChannel reads <dir>/<channel>/{node.conf,encryption-key}.
func readChannel(dir, channel string) (*channelInfo, error) {
	base := filepath.Join(dir, channel)
	keyBytes, err := os.ReadFile(filepath.Join(base, "encryption-key"))
	if err != nil {
		return nil, fmt.Errorf("channel %s: %w", channel, err)
	}
	f, err := os.Open(filepath.Join(base, "node.conf"))
	if err != nil {
		return nil, fmt.Errorf("channel %s: %w", channel, err)
	}
	defer f.Close()

	info := &channelInfo{key: strings.TrimSpace(string(keyBytes))}
	var section, typ, url, listen string
	flush := func() {
		switch {
		case section == "":
		case typ == "direct":
			if _, p, err := net.SplitHostPort(listen); err == nil {
				info.port, _ = strconv.Atoi(p)
			}
		case share.Shareable(typ) && url != "":
			info.transports = append(info.transports, provision.ChannelTransport{Type: typ, URL: url})
		}
		typ, url, listen = "", "", ""
	}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "["):
			flush()
			section = strings.Trim(line, "[]")
		case strings.HasPrefix(line, "#") || line == "":
		default:
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			k, v = strings.TrimSpace(k), strings.TrimSpace(v)
			switch {
			case section == "" && k == "URL":
				info.context = v
			case k == "Type":
				typ = v
			case k == "URL":
				url = v
			case k == "Listen":
				listen = v
			}
		}
	}
	flush()
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(info.key) != 64 || info.port == 0 || len(info.transports) == 0 {
		return nil, fmt.Errorf("channel %s: no key, direct port or documents in its config", channel)
	}
	return info, nil
}

// transportsHeader is where an app lists the transport types it runs
// ("yandex,vyandex,telemost"), so its links carry only those.
const transportsHeader = "X-OpenFlux-Transports"

// transportSet is the transport types one app runs; nil is every type a link
// can carry (the owner's checks).
type transportSet map[string]bool

// legacyTransports are what an app that does not send transportsHeader can
// read: the types links carried before it existed. An app drops a link with a
// type it does not know, the whole location with it, so it never gets more.
var legacyTransports = transportSet{"vyandex": true, "yandex": true, "mailru": true, "boards": true, "cupsonline": true}

func (t transportSet) allows(typ string) bool { return t == nil || t[typ] }

// key names the set in the link cache.
func (t transportSet) key() string {
	if t == nil {
		return "*"
	}
	names := make([]string, 0, len(t))
	for n := range t {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

// clientTransports is what the app said it runs. Entries a link cannot carry
// are ignored (script:<id>@<wire> is kept for script packages, not read yet).
func clientTransports(r *http.Request) transportSet {
	v := strings.Join(r.Header.Values(transportsHeader), ",")
	if strings.TrimSpace(v) == "" {
		return legacyTransports
	}
	if len(v) > 1024 {
		v = v[:1024]
	}
	set := transportSet{}
	for i, name := range strings.Split(v, ",") {
		if i == 32 {
			break
		}
		name = strings.ToLower(strings.TrimSpace(name))
		if share.Shareable(name) {
			set[name] = true
		}
	}
	return set
}

// errNoTransports: the channel is fine, but has none of the app's transports.
var errNoTransports = errors.New("no transport the app runs")

// linkCache reads each channel once and builds its openflux:// link for each
// set of transports, again when the channel's files change, so a transport
// changed in the app, or a rotated key, reaches subscribers on their next refresh.
type linkCache struct {
	mu      sync.Mutex
	dir     string
	entries map[string]cached
}

type cached struct {
	conf, key time.Time
	info      *channelInfo
	links     map[string]string // by transportSet.key
}

func newLinkCache(dir string) *linkCache { return &linkCache{dir: dir, entries: map[string]cached{}} }

// link is the pool's link with the channel's transports of set; errNoTransports
// when the channel has none of them.
func (c *linkCache) link(p Pool, channel string, set transportSet) (string, error) {
	base := filepath.Join(c.dir, channel)
	ci, err := os.Stat(filepath.Join(base, "node.conf"))
	if err != nil {
		return "", err
	}
	ki, err := os.Stat(filepath.Join(base, "encryption-key"))
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[channel]
	if !ok || !e.conf.Equal(ci.ModTime()) || !e.key.Equal(ki.ModTime()) {
		info, err := readChannel(c.dir, channel)
		if err != nil {
			return "", err
		}
		e = cached{conf: ci.ModTime(), key: ki.ModTime(), info: info, links: map[string]string{}}
		c.entries[channel] = e
	}
	if link, ok := e.links[set.key()]; ok {
		return link, nil
	}
	var ts []provision.ChannelTransport
	for _, t := range e.info.transports {
		if set.allows(t.Type) {
			ts = append(ts, t)
		}
	}
	if len(ts) == 0 {
		return "", errNoTransports
	}
	// The context is the channel's, whichever of its documents this app gets.
	context := e.info.context
	if context == "" {
		all, err := provision.CheckTransports(e.info.transports)
		if err != nil {
			return "", err
		}
		context = provision.SessionContext(all)
	}
	link, err := provision.ShareLinkContext(p.Name, e.info.key, p.Host, e.info.port, ts, context)
	if err != nil {
		return "", err
	}
	e.links[set.key()] = link
	return link, nil
}

// profiles are the locations an owner (account or key) may use, with the
// transports of set. A perUser pool gives each owner their own channel; a pool
// whose files cannot be read, or without any transport of set, is left out:
// the others still work.
func (s *Server) profiles(poolIDs []string, owner string, set transportSet) []subscription.Profile {
	want := map[string]bool{}
	for _, id := range poolIDs {
		want[id] = true
	}
	// out starts empty, never nil: the JSON of a document with no usable pool
	// must be "profiles":[], which the apps read, and not "profiles":null.
	out := []subscription.Profile{}
	for _, p := range s.cfg.Pools {
		if len(want) > 0 && !want[p.ID] {
			continue
		}
		channel := p.Channel
		if p.PerUser {
			if owner == "" {
				continue
			}
			channel = userChannel(owner)
		}
		link, err := s.links.link(p, channel, set)
		if errors.Is(err, errNoTransports) {
			continue
		}
		if err != nil {
			s.logf("pool %s: %v", p.ID, err)
			if p.AutoNode || p.PerUser {
				s.start(nodeRequest{Pool: p, Channel: channel, Owner: owner})
			}
			continue
		}
		out = append(out, subscription.Profile{ID: p.ID, Name: p.Name, Link: link})
	}
	return out
}
