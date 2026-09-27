package mobile

import (
	"encoding/json"
	"fmt"

	"openflux/transport"
	"openflux/transport/manager"
)

// sessionSpec is one transport of a Session profile, as the app sends it.
// Names must match the exit's (the CLI names --transports entries after
// their type) because cookie exchange is addressed by name.
type sessionSpec struct {
	Name     string                 `json:"name"`
	Type     string                 `json:"type"`
	URL      string                 `json:"url"`
	Priority int                    `json:"priority"`
	Params   map[string]interface{} `json:"params"`
}

// parseSessionSpecs reads what the app sends: a JSON array of sessionSpec,
// or {"context": ..., "transports": [...]} when the profile carries the
// exit's encryption context explicitly (imported from an openflux:// link).
// Without one the context is derived with sessionContext.
func parseSessionSpecs(specsJSON string) ([]sessionSpec, string, error) {
	var wrapped struct {
		Context    string        `json:"context"`
		Transports []sessionSpec `json:"transports"`
	}
	var specs []sessionSpec
	if err := json.Unmarshal([]byte(specsJSON), &specs); err != nil {
		if err := json.Unmarshal([]byte(specsJSON), &wrapped); err != nil {
			return nil, "", fmt.Errorf("список транспортов: %w", err)
		}
		specs = wrapped.Transports
	}
	if len(specs) == 0 {
		return nil, "", fmt.Errorf("список транспортов пуст")
	}
	context := wrapped.Context
	if context == "" {
		context = sessionContext(specs)
	}
	return specs, context, nil
}

// buildSession mirrors the CLI client's --negotiate / --transports path, so
// the phone talks to an exit started with the same transports and --url.
// specsJSON is what parseSessionSpecs reads. exit builds the exit node's
// side (the phone as an l4 exit).
func buildSession(specsJSON, secret string, exit bool) (transport.Transport, error) {
	t, _, err := buildSessionWith(specsJSON, secret, exit)
	return t, err
}

// buildSessionWith is buildSession that also returns the Session, for
// callers that need per-transport state.
func buildSessionWith(specsJSON, secret string, exit bool) (transport.Transport, *transport.Session, error) {
	specs, context, err := parseSessionSpecs(specsJSON)
	if err != nil {
		return nil, nil, err
	}
	if len(secret) < 16 {
		return nil, nil, fmt.Errorf("для режима Session нужен ключ шифрования не короче 16 символов")
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
		}
		if err := sess.AddTransport(spec.Name, raw, secret, context, spec.Priority); err != nil {
			return nil, nil, err
		}
		provider, _ := raw.(manager.CookieProvider)
		if err := m.Add(spec.Name, spec.Type, raw, spec.Priority, provider); err != nil {
			return nil, nil, err
		}
		if provider != nil {
			keys[spec.Name] = spec.Type + " " + spec.URL
		}
		appendLog(fmt.Sprintf("[ANDROID] Session: транспорт %s (%s), приоритет %d", spec.Name, spec.Type, spec.Priority))
	}
	sess.SetControlHandler(m.DispatchControl)
	setSessionRoute(sess, types)
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

// sessionContext is the encryption context, derived as the core's
// pickSessionContext does for an exit without --url: the document URL of the
// highest-priority transport that has one, cupsonline aside (its room list
// only exists once the exit is up), else "http://#". Equal priorities keep
// the first transport, as the core does.
func sessionContext(specs []sessionSpec) string {
	best := -1
	for i, s := range specs {
		if s.Type == "cupsonline" || s.URL == "" || s.URL == placeholderURL {
			continue
		}
		if best < 0 || s.Priority > specs[best].Priority {
			best = i
		}
	}
	if best >= 0 {
		return specs[best].URL
	}
	return placeholderURL
}

// placeholderURL is the core's --url default, the context of a channel that
// has no document URL.
const placeholderURL = "http://#"
