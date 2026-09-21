package main

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"openflux/transport"
)

// splitURLs splits a comma-separated --url value into document URLs, trimmed,
// de-duplicated and sorted. Sorting gives both peers the same stream order
// whatever order the URLs were typed in, so they route a connection over the
// same document. Duplicates must go: two sessions of one peer in the same
// document would receive each other's packets. An empty value yields [""] so
// callers can always use urls[0].
func splitURLs(raw string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, p := range strings.Split(raw, ",") {
		p = strings.TrimSpace(p)
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return []string{""}
	}
	sort.Strings(out)
	return out
}

// supportsMultiStream reports whether a transport takes one document per
// stream. cupsonline carries its rooms inside one URL, oneme has no URL, and
// mailru has not been tested with several documents.
func supportsMultiStream(transportType string) bool {
	return transportType == "yandex" || transportType == "vyandex"
}

// newDocStreams builds the transport for urls: the stream itself for a single
// URL (exactly the single-document behavior), or a MultiStreamTransport over
// one complete stream per document. mk builds the stream for one URL.
func newDocStreams(urls []string, mk func(url string) transport.Transport) transport.Transport {
	if len(urls) <= 1 {
		return mk(urls[0])
	}
	log.Printf("Multi-stream: %d documents", len(urls))
	streams := make([]transport.Transport, 0, len(urls))
	for _, u := range urls {
		streams = append(streams, mk(u))
	}
	return transport.NewMultiStreamTransport(streams)
}

// multistreamStatusLoop logs one line per interval with each document's state:
// UP (connected, peer heard from recently), NOPEER (connected, peer silent)
// or DOWN.
func multistreamStatusLoop(ms *transport.MultiStreamTransport, urls []string, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for range tick.C {
		streams := ms.Streams()
		parts := make([]string, 0, len(streams))
		up, alive := 0, 0
		for i, s := range streams {
			st := s.Stats()
			state := "DOWN"
			if st.Connected {
				up++
				state = "NOPEER"
				if ms.PeerAlive(i) {
					alive++
					state = "UP"
				}
			}
			peer := "never"
			if !st.LastRecv.IsZero() {
				peer = time.Since(st.LastRecv).Round(time.Second).String()
			}
			parts = append(parts, fmt.Sprintf("s%d[%s]=%s(peer=%s,rx=%d,tx=%d,rc=%d)",
				i, docLabel(urls[i]), state, peer, st.PacketsRecv, st.PacketsSent, st.Reconnects))
		}
		log.Printf("[MULTI] connected=%d/%d peer=%d/%d %s",
			up, len(streams), alive, len(streams), strings.Join(parts, " "))
	}
}

// docLabel shortens a document URL to its last path segment for logs.
func docLabel(u string) string {
	u = strings.TrimRight(u, "/")
	if i := strings.LastIndex(u, "/"); i >= 0 {
		u = u[i+1:]
	}
	if len(u) > 8 {
		u = u[:8]
	}
	return u
}
