package main

import (
	"fmt"
	"strconv"
	"strings"

	"openflux/transport"
	"openflux/transport/control"
	"openflux/transport/manager"
)

// transportSpec is one entry from --transports plus its per-type URL/params.
type transportSpec struct {
	Name     string
	Type     string
	Priority int
	URL      string
	Params   map[string]interface{}
}

// parseTransportList parses "direct:100,yandex:50,mailru:30".
// Priority is optional; default is 50.
func parseTransportList(s string) ([]transportSpec, error) {
	var out []transportSpec
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, prioStr, hasPrio := strings.Cut(part, ":")
		prio := 50
		if hasPrio {
			v, err := strconv.Atoi(prioStr)
			if err != nil {
				return nil, fmt.Errorf("bad priority %q: %w", prioStr, err)
			}
			prio = v
		}
		out = append(out, transportSpec{
			Name:     name,
			Type:     name, // by default the name and the type match
			Priority: prio,
		})
	}
	return out, nil
}

// buildTransportSpecs assembles the config for each name in the list, using
// per-type URL flags (--yandex-url, --mailru-url, ...) and global settings.
func buildTransportSpecs(specs []transportSpec, urls map[string]string, extra map[string]map[string]interface{}) []transportSpec {
	for i := range specs {
		if u, ok := urls[specs[i].Type]; ok {
			specs[i].URL = u
		}
		if p, ok := extra[specs[i].Type]; ok {
			specs[i].Params = p
		}
	}
	return specs
}

// makeRawTransport builds a raw transport from a spec without the Manager's
// factory (bootstrap path). The Manager's factory is only used for transports
// added later via SubtypeTransportStart.
func makeRawTransport(spec transportSpec, baseCfg transport.TransportConfig) (transport.Transport, error) {
	cfg := &control.TransportConfig{
		Name:   spec.Name,
		Type:   spec.Type,
		URL:    spec.URL,
		Params: spec.Params,
	}
	return transportFactory(baseCfg)(cfg)
}

// registerBootstrapTransports wires every spec into the manager, and also
// calls Session.AddTransport with the shared secret/context so the handshake
// can use any of them.
func registerBootstrapTransports(m *manager.Manager, specs []transportSpec, baseCfg transport.TransportConfig, secret, ctx string) error {
	for _, spec := range specs {
		raw, err := makeRawTransport(spec, baseCfg)
		if err != nil {
			return fmt.Errorf("%s: %w", spec.Name, err)
		}
		var provider manager.CookieProvider
		if p, ok := raw.(manager.CookieProvider); ok {
			provider = p
		}

		// Session-side: wrap with Encrypted+Batched and register for handshake.
		if err := m.Session().AddTransport(spec.Name, raw, secret, ctx, spec.Priority); err != nil {
			return fmt.Errorf("session add %s: %w", spec.Name, err)
		}
		// Manager-side: keep the raw pointer and its cookie provider.
		if err := m.Add(spec.Name, spec.Type, raw, spec.Priority, provider); err != nil {
			return fmt.Errorf("manager add %s: %w", spec.Name, err)
		}
	}
	return nil
}
