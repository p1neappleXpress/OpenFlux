package mobile

import (
	"encoding/json"
	"fmt"

	"github.com/p1neappleXpress/OpenFlux/transport"
	"github.com/p1neappleXpress/OpenFlux/transport/manager"
	"github.com/p1neappleXpress/OpenFlux/utils"
)

// sessionSpec is one transport of a Session profile, as the app sends it.
// Names must match the exit's (the CLI names --transports entries after
// their type) because cookie exchange is addressed by name; the Manager
// also matches carriers by document URL and type when they differ.
type sessionSpec struct {
	Name     string                 `json:"name"`
	Type     string                 `json:"type"`
	URL      string                 `json:"url"`
	Priority int                    `json:"priority"`
	Params   map[string]interface{} `json:"params"`
}

// sessionOptions tunes buildSessionWith beyond the specs.
type sessionOptions struct {
	// classic enables the classic layering next to the Session (see
	// transport.Session.SetClassic): a classic profile's client falls back
	// to it, a classic exit serves classic clients. codec is its
	// preferred framing.
	classic bool
	codec   string
	// strict turns the classic fallback a single-carrier Session client
	// gets by default off.
	strict bool
}

// parseSessionSpecs reads what the app sends: a JSON array of sessionSpec,
// or {"context": ..., "transports": [...]} when the profile carries the
// exit's encryption context explicitly (imported from an openflux:// link).
// The context is picked by transport.KDFContexts, the rule every peer uses;
// alternates are what other builds may have derived instead.
func parseSessionSpecs(specsJSON string) (specs []sessionSpec, context string, alternates []string, err error) {
	var wrapped struct {
		Context    string        `json:"context"`
		Transports []sessionSpec `json:"transports"`
	}
	if err := json.Unmarshal([]byte(specsJSON), &specs); err != nil {
		if err := json.Unmarshal([]byte(specsJSON), &wrapped); err != nil {
			return nil, "", nil, fmt.Errorf("список транспортов: %w", err)
		}
		specs = wrapped.Transports
	}
	if len(specs) == 0 {
		return nil, "", nil, fmt.Errorf("список транспортов пуст")
	}
	seen := make(map[string]int)
	for i := range specs {
		if specs[i].Name == "" {
			seen[specs[i].Type]++
			specs[i].Name = specs[i].Type
			if n := seen[specs[i].Type]; n > 1 {
				specs[i].Name = fmt.Sprintf("%s-%d", specs[i].Type, n)
			}
		}
	}
	context, alternates = transport.KDFContexts(wrapped.Context, "", contextSources(specs))
	return specs, context, alternates, nil
}

func contextSources(specs []sessionSpec) []transport.ContextSource {
	out := make([]transport.ContextSource, len(specs))
	for i, s := range specs {
		url := s.URL
		if s.Type == "direct" {
			url = ""
		}
		out[i] = transport.ContextSource{Type: s.Type, URL: url, Priority: s.Priority}
	}
	return out
}

// buildSession mirrors the CLI client's --negotiate / --transports path, so
// the phone talks to an exit started with the same transports and --url.
// specsJSON is what parseSessionSpecs reads. exit builds the exit node's
// side (the phone as an l4 exit).
func buildSession(specsJSON, secret string, exit bool) (transport.Transport, error) {
	t, _, err := buildSessionWith(specsJSON, secret, exit, sessionOptions{})
	return t, err
}

// buildSessionWith is buildSession that also returns the Session, for
// callers that need per-transport state.
func buildSessionWith(specsJSON, secret string, exit bool, opt sessionOptions) (transport.Transport, *transport.Session, error) {
	specs, context, alternates, err := parseSessionSpecs(specsJSON)
	if err != nil {
		return nil, nil, err
	}
	if utils.SecretChars(secret) < utils.MinSecretChars {
		return nil, nil, fmt.Errorf("для режима Session нужен ключ шифрования не короче %d символов", utils.MinSecretChars)
	}

	// Like the CLI: an l4 exit terminates flows in gVisor and has no raw
	// ICMP errors to relay; a client does.
	caps := transport.CapabilityIPv4 | transport.CapabilityTCP | transport.CapabilityUDP
	if !exit {
		caps |= transport.CapabilityICMPErrors
	}
	sess, err := transport.NewSession(transport.PeerParameters{
		Capabilities:  caps,
		MaxPacketSize: transport.MaxNegotiatedPacket,
	}, exit)
	if err != nil {
		return nil, nil, err
	}
	// A single-carrier client falls back to the classic layering while the
	// exit does not answer the handshake (an exit that predates Session or
	// runs classic) and upgrades once it does. A classic exit also serves
	// classic clients.
	switch {
	case opt.classic:
		sess.SetClassic(opt.codec)
	case !exit && !opt.strict && len(specs) == 1:
		sess.SetClassic(transport.CodecBatched)
	}
	sess.SetAlternateContexts(alternates)
	appendLog(fmt.Sprintf("[ANDROID] Session: контекст шифрования sha256 %s (запасных: %d)",
		utils.Sha256Short([]byte(context)), len(alternates)))

	m := manager.New(sess, nil, secret, context)
	config := transport.DefaultConfig()
	keys := make(map[string]string)
	types := make(map[string]string)
	for _, spec := range specs {
		types[spec.Name] = spec.Type
		raw, err := newRawTransport(spec.Type, spec.URL, spec.Params, config, exit)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", spec.Name, err)
		}
		if exit {
			addExitRoom(spec.Name, raw)
			// A cupsonline exit learns its rooms at start; older classic
			// clients derived their key from that list.
			if r, ok := raw.(interface{ OnRoomList(func(string)) }); ok {
				r.OnRoomList(func(packed string) {
					if packed != "" {
						sess.SetAlternateContexts([]string{packed})
					}
				})
			}
		}
		if err := sess.AddTransport(spec.Name, raw, secret, context, spec.Priority); err != nil {
			return nil, nil, err
		}
		provider, _ := raw.(manager.CookieProvider)
		if err := m.Add(spec.Name, spec.Type, raw, spec.Priority, provider); err != nil {
			return nil, nil, err
		}
		if spec.Type != "direct" && spec.Type != "oneme" {
			m.SetURL(spec.Name, spec.URL)
		}
		if provider != nil {
			keys[spec.Name] = spec.Type + " " + spec.URL
		}
		appendLog(fmt.Sprintf("[ANDROID] Session: транспорт %s (%s), приоритет %d", spec.Name, spec.Type, spec.Priority))
	}
	sess.SetControlHandler(m.DispatchControl)
	setSessionRoute(sess, types)
	applyInitialCookies(m, specs)
	if exit {
		// The exit relays its own checks to the client itself (AuthRequired);
		// the phone's UI can still pass them locally.
		attachSessionCaptcha(m, keys, nil)
		appendLog("[ANDROID] Session: шифрование AES-256-GCM, ожидание клиента")
		return m, sess, nil
	}

	// The side stack for exit checks shares the tunnel; a PortDemux hands
	// it the replies to its ports and everything else to the regular path.
	demux := transport.NewPortDemux(m, authProxyPortLo, authProxyPortHi)
	proxy := &authProxy{demux: demux}
	setAuthProxy(proxy)
	attachSessionCaptcha(m, keys, proxy)
	appendLog("[ANDROID] Session: шифрование AES-256-GCM, согласование с нодой")
	return demux, sess, nil
}
