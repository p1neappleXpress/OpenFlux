package transport

import "sort"

// ContextPlaceholder is the --url default and the one fixed encryption KDF
// context every current peer uses unless a context was set explicitly. One
// string for every carrier and every mode: the peers cannot diverge on it.
const ContextPlaceholder = "openflux_"

// legacyPlaceholder is the placeholder older builds use. It stays an
// alternate, so a build in the field is still reached.
const legacyPlaceholder = "http://#"

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

// KDFContexts returns the context a peer derives its encryption keys from (the
// scrypt salt) and the alternates it will also try.
//
// The PRIMARY is pinned (see EncryptedTransport and the pin commit): an
// explicit context — set with --session-context, or carried by an openflux://
// link — is shared out of band so both peers hold the same string and cannot
// diverge; otherwise every current peer, build and carrier uses the one fixed
// ContextPlaceholder. Two up-to-date peers that share the secret therefore
// derive the same keys on the first try, with no divergence to work around.
//
// The ALTERNATES are the LEGACY fallback. Before the pin, a peer computed its
// context FROM the carrier URLs and transport names, and the two sides had to
// independently arrive at the same string (it is only a public salt — secrecy
// comes from the secret). Builds still deployed in the field do exactly that,
// so to interoperate with them we offer those same strings as alternates: a
// receiver that cannot authenticate a packet under the pinned primary tries
// them before dropping it, and the sender rotates through them while the peer
// stays silent (see EncryptedTransport). An up-to-date peer never needs them —
// both sides agree on the primary; an older peer whose primary is a URL/room
// list is reached through them. The candidate order, most likely first, is the
// context an old build would have derived as ITS primary, then the placeholder,
// --url taken literally, every carrier URL, and the transport names.
//
// globalURL and specs feed only the legacy alternates now; the primary ignores
// them unless explicit is set.
func KDFContexts(explicit, globalURL string, specs []ContextSource) (primary string, alternates []string) {
	if explicit != "" {
		primary = explicit
	} else {
		primary = ContextPlaceholder
	}

	// legacyPrimary is the context a pre-pin build would have chosen as its
	// own primary for this same setup; it is the alternate most worth trying
	// first when talking to the deployed fleet.
	cups := false
	for _, s := range specs {
		if s.Type == "cupsonline" && s.URL != "" && s.URL == globalURL {
			cups = true
		}
	}
	if len(specs) == 1 && specs[0].Type == "cupsonline" {
		cups = true
	}
	var legacyPrimary string
	switch {
	case explicit != "":
		legacyPrimary = explicit
	case globalURL != "" && globalURL != ContextPlaceholder && !cups:
		legacyPrimary = globalURL
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
			legacyPrimary = specs[best].URL
		} else {
			legacyPrimary = ContextPlaceholder
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
	add(legacyPrimary)
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
