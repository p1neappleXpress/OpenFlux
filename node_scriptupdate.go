package main

import (
	"context"
	"path/filepath"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport"
	"github.com/p1neappleXpress/OpenFlux/transport/manager"
	"github.com/p1neappleXpress/OpenFlux/transport/script"
	"github.com/p1neappleXpress/OpenFlux/utils"
)

// startNodeScriptUpdater wires the Go-side auto-updater into a running node: it
// keeps every updatable .flux carrier current (the same auto-OK rule the app's
// ScriptUpdater uses) and drives each carrier's recovery ladder, hot-swapping a
// reloaded package into the live Session with no restart. It is the headless
// counterpart of the GUI apps' Kotlin ScriptUpdater - a CLI/exit node has no
// GUI, so this is the only updater there. Enabled by --script-auto-update.
//
// ctx bounds the updater's goroutines (process lifetime on a node). Returns how
// many transports were enrolled.
func startNodeScriptUpdater(ctx context.Context, m *manager.Manager, specs []transportSpec, baseCfg transport.TransportConfig, channel string) int {
	isExit := m.Session().IsExit()

	type carrier struct {
		spec transportSpec
		inst script.Installed
	}
	byID := map[string]carrier{}
	dir := ""
	for _, spec := range specs {
		if spec.Type != "script" {
			continue
		}
		st, ok := m.Raw(spec.Name).(*script.ScriptTransport)
		if !ok {
			continue
		}
		inst, ok := st.Installed()
		if !ok {
			continue // a plain .js, or a package with no update source
		}
		byID[inst.ID] = carrier{spec: spec, inst: inst}
		if dir == "" {
			if p, _ := spec.Params["path"].(string); p != "" {
				dir = filepath.Dir(p)
			}
		}
	}
	if len(byID) == 0 {
		return 0
	}

	u := script.NewAutoUpdater(script.AutoUpdaterConfig{
		Dir:     dir,
		Channel: channel,
		Auto:    true, // the --script-auto-update flag gates whether we get here
		Reload: func(id string) {
			c, ok := byID[id]
			if !ok {
				return
			}
			spec := c.spec
			// Rebuild from the spec; makeRawTransport re-reads the .flux now on
			// disk (the just-applied version), then hot-swap it into the Session.
			if err := m.ReloadWith(spec.Name, func() (transport.Transport, error) {
				return makeRawTransport(spec, baseCfg, isExit)
			}); err != nil {
				utils.Infof("[SCRIPT-UPDATE] reload %q failed: %v", id, err)
			}
		},
	})

	for id, c := range byID {
		u.Enroll(c.inst)
		name := c.spec.Name
		// Probe the live transport by name so the watch follows a hot-swap
		// instead of polling a stopped instance.
		go u.Watch(ctx, id, func() bool {
			r := m.Raw(name)
			return r != nil && r.IsConnected()
		}, 5*time.Second, 30*time.Second)
	}
	go u.Run(ctx, 24*time.Hour)
	utils.Infof("[SCRIPT-UPDATE] node auto-update on: %d transport(s), channel=%s", len(byID), channel)
	return len(byID)
}
