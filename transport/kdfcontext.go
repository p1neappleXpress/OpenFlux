package transport

import "sort"

// ContextPlaceholder is the --url default and the encryption context of a
// channel none of whose carriers has a document URL.
const ContextPlaceholder = "http://#"

// maxContextCandidates bounds how many contexts a peer tries: each one costs
// an scrypt derivation (32 MiB, tens of milliseconds) the first time it is
// needed.
const maxContextCandidates = 8

// ContextSource is one carrier as far as the encryption context cares.
type ContextSource struct {
	Type     string
	URL      string
	Priority int
}

// contextURL reports whether a carrier's URL can name the channel. A
// cupsonline "URL" is the room list its exit creates at start, so the exit
// cannot know it beforehand; direct carries host:port, which differs between
// the two sides (0.0.0.0:port on the exit, the public address on the
// client); oneme has no URL.
func contextURL(s ContextSource) bool {
	switch s.Type {
	case "cupsonline", "direct", "oneme":
		return false
	}
	return s.URL != "" && s.URL != ContextPlaceholder
}

// KDFContexts is the one rule every OpenFlux peer uses to pick the context
// its encryption keys are derived from (the scrypt salt); both peers must
// arrive at the same string. It replaces the copies that had drifted apart
// (CLI, Android bridge, Desktop, iOS):
//
//	explicit      --session-context / the context an openflux:// link carries
//	--url         unless it is a cupsonline room list
//	transports    URL of the highest-priority carrier that names the
//	              channel (see contextURL); equal priorities keep the first
//	fallback      ContextPlaceholder
//
// alternates are the contexts other or older builds derive for the same
// setup, most likely first: the placeholder, --url taken literally (older
// classic cupsonline peers used the room list), every carrier URL, and the
// transport names (third-party panels and an older fork used them when no
// URL was set). A receiver that cannot authenticate a packet under the
// primary context tries these before dropping it; see EncryptedTransport.
func KDFContexts(explicit, globalURL string, specs []ContextSource) (primary string, alternates []string) {
	cups := false
	for _, s := range specs {
		if s.Type == "cupsonline" && s.URL != "" && s.URL == globalURL {
			cups = true
		}
	}
	if len(specs) == 1 && specs[0].Type == "cupsonline" {
		cups = true
	}
	switch {
	case explicit != "":
		primary = explicit
	case globalURL != "" && globalURL != ContextPlaceholder && !cups:
		primary = globalURL
	default:
		best := -1
		for i, s := range specs {
			if !contextURL(s) {
				continue
			}
			if best < 0 || s.Priority > specs[best].Priority {
				best = i
			}
		}
		if best >= 0 {
			primary = specs[best].URL
		} else {
			primary = ContextPlaceholder
		}
	}

	seen := map[string]bool{primary: true}
	add := func(c string) {
		if c == "" || seen[c] || len(alternates) >= maxContextCandidates-1 {
			return
		}
		seen[c] = true
		alternates = append(alternates, c)
	}
	add(ContextPlaceholder)
	if globalURL != ContextPlaceholder {
		add(globalURL)
	}
	byPriority := append([]ContextSource(nil), specs...)
	sort.SliceStable(byPriority, func(i, j int) bool { return byPriority[i].Priority > byPriority[j].Priority })
	for _, s := range byPriority {
		if s.Type != "direct" && s.Type != "oneme" {
			add(s.URL)
		}
	}
	for _, s := range byPriority {
		add(s.Type)
	}
	return primary, alternates
}
