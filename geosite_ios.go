//go:build ios

package main

// GeoSite split tunneling (Phase 2). The packet-tunnel extension already proxies
// every device DNS query over DoT, so it sees both the queried domain and the
// resolved IPs. When a domain matches the geosite "direct" set, its answer IPs
// are recorded and handed to the Swift provider, which adds them to the tunnel's
// excludedRoutes so that domain's traffic goes direct — even when the IP is
// foreign (e.g. a Russian service on a foreign CDN) and thus not in the GeoIP RU
// set. This is the only way to route by domain on iOS, where the OS routes L3
// traffic and "direct" means "excluded from the tunnel".

import (
	"net"
	"strings"
	"sync"

	"golang.org/x/net/dns/dnsmessage"
)

var (
	geoMu      sync.RWMutex
	geoSet     map[string]struct{} // domain suffixes that go direct
	directSeen = map[string]struct{}{}
	directPend []string // IPs not yet handed to the Swift side
)

// setGeositeDirect loads the newline-separated domain-suffix list (one domain per
// line; leading "*." / "." and comments are tolerated).
func setGeositeDirect(list string) {
	set := make(map[string]struct{})
	for _, ln := range strings.Split(list, "\n") {
		ln = strings.TrimSpace(strings.ToLower(ln))
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		ln = strings.TrimPrefix(ln, "*.")
		ln = strings.TrimPrefix(ln, ".")
		ln = strings.TrimSuffix(ln, ".")
		if ln != "" {
			set[ln] = struct{}{}
		}
	}
	geoMu.Lock()
	geoSet = set
	geoMu.Unlock()
}

// geoMatch reports whether name (or any parent domain of it) is in the direct
// set. Walks label boundaries, so it is O(labels) per query regardless of set
// size.
func geoMatch(name string) bool {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	geoMu.RLock()
	defer geoMu.RUnlock()
	if len(geoSet) == 0 {
		return false
	}
	for {
		if _, ok := geoSet[name]; ok {
			return true
		}
		i := strings.IndexByte(name, '.')
		if i < 0 {
			return false
		}
		name = name[i+1:]
	}
}

// geositeNoteAnswer parses the DNS query name and answer A-records; if the name
// is a geosite-direct domain, records its IPv4 answers as pending direct routes.
// Best-effort: any parse error is silently ignored.
func geositeNoteAnswer(query, answer []byte) {
	defer func() { _ = recover() }()

	var pq dnsmessage.Parser
	if _, err := pq.Start(query); err != nil {
		return
	}
	q, err := pq.Question()
	if err != nil {
		return
	}
	if !geoMatch(q.Name.String()) {
		return
	}

	var pa dnsmessage.Parser
	if _, err := pa.Start(answer); err != nil {
		return
	}
	if err := pa.SkipAllQuestions(); err != nil {
		return
	}
	var ips []string
	for {
		h, err := pa.AnswerHeader()
		if err != nil {
			break
		}
		if h.Type == dnsmessage.TypeA {
			r, err := pa.AResource()
			if err != nil {
				break
			}
			ips = append(ips, net.IP(r.A[:]).String())
		} else {
			if err := pa.SkipAnswer(); err != nil {
				break
			}
		}
	}
	if len(ips) == 0 {
		return
	}
	geoMu.Lock()
	for _, ip := range ips {
		if _, ok := directSeen[ip]; !ok {
			directSeen[ip] = struct{}{}
			directPend = append(directPend, ip)
		}
	}
	geoMu.Unlock()
}

// drainDirectIPsCapped returns as many pending direct IPs (newline-joined) as fit
// in max bytes and re-queues the rest for the next drain.
func drainDirectIPsCapped(max int) string {
	geoMu.Lock()
	defer geoMu.Unlock()
	if len(directPend) == 0 || max <= 0 {
		return ""
	}
	var b strings.Builder
	i := 0
	for ; i < len(directPend); i++ {
		extra := len(directPend[i])
		if b.Len() > 0 {
			extra++ // newline
		}
		if b.Len()+extra > max {
			break
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(directPend[i])
	}
	directPend = directPend[i:]
	return b.String()
}
