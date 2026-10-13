package script

import (
	"context"
	"sync"
	"time"

	"github.com/p1neappleXpress/OpenFlux/utils"
)

// AutoUpdater keeps a node's installed .flux transports current and drives the
// recovery ladder for each. It is the Go counterpart of the app's Kotlin
// ScriptUpdater: a node (an exit) runs the same transports a client does, so a
// published fix has to reach the node too, or a doc carrier whose client
// updated still cannot come up because the exit speaks the old wire.
//
// It owns no transports and builds none. Each enrolled transport is described
// by its Installed record; when an update is applied or a package rolled back,
// the AutoUpdater calls the Reload callback and it is the caller (whoever owns
// the Session) that tears the live link down and rebuilds it from the new file
// on disk. Fallback to a native transport is likewise only signalled, never
// performed here. This keeps all the file/network work and the ladder in one
// tested place and leaves lifecycle to the one layer that owns it.
//
// Everything is reported as machine codes through the logger; there is no user
// text in this package.
type AutoUpdater struct {
	cfg   AutoUpdaterConfig
	fetch Fetcher
	now   func() time.Time

	mu      sync.Mutex
	entries map[string]*updateEntry
	// One action runs at a time per id; a step requested while one is in
	// flight is held in pending (not dropped) and run next. The ladder only
	// ever climbs, so the latest request is always the highest, and keeping
	// just the latest both coalesces a burst and never loses an escalation.
	running map[string]bool
	pending map[string]RecoveryAction
}

// AutoUpdaterConfig is the fixed configuration of an AutoUpdater.
type AutoUpdaterConfig struct {
	// Dir is the scripts directory the .flux files live in and updates install
	// into (the previous version kept as <file>.prev).
	Dir string
	// Channel is the update channel ("stable" by default, "nightly" for a node
	// tracking pre-releases).
	Channel string
	// Auto, when true, lets the updater install auto-OK updates (official key,
	// same wire, same key) and roll back on its own. When false it still runs
	// the ladder and logs what it would do, but changes nothing on disk - a
	// node operator who wants every change by hand sets it false.
	Auto bool
	// Policy is the recovery ladder's thresholds; zero fields take the default.
	Policy RecoveryPolicy

	// Fetch overrides the HTTP fetcher (tests, or a node routing through its
	// own client); nil uses HTTPFetcher.
	Fetch Fetcher
	// Now overrides the clock (tests); nil uses time.Now.
	Now func() time.Time

	// Reload is called with an id after its package changed on disk (an update
	// installed or a rollback): the caller rebuilds the live link from the new
	// file. May be nil (nothing to reload).
	Reload func(id string)
	// Fallback is called with an id when the ladder gives up on the scripted
	// path entirely: the caller may hand the carrier to the native transport.
	// May be nil (no native fallback available for this node).
	Fallback func(id string)
}

type updateEntry struct {
	inst   Installed
	ladder *RecoveryLadder
}

// NewAutoUpdater builds an updater from cfg. It does not start any loop; call
// Run for the periodic check, and OnConnected/OnFailed from the supervisor.
func NewAutoUpdater(cfg AutoUpdaterConfig) *AutoUpdater {
	if cfg.Channel == "" {
		cfg.Channel = "stable"
	}
	cfg.Policy = cfg.Policy.Normalize()
	fetch := cfg.Fetch
	if fetch == nil {
		fetch = HTTPFetcher
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &AutoUpdater{
		cfg:     cfg,
		fetch:   fetch,
		now:     now,
		entries: map[string]*updateEntry{},
		running: map[string]bool{},
		pending: map[string]RecoveryAction{},
	}
}

// Enroll registers or refreshes the update record for a transport and gives it
// a fresh recovery ladder. Call it whenever a (re)built transport reports an
// Installed record; a plain .js or an un-updatable package is simply not
// enrolled (its Installed() returned ok=false).
func (u *AutoUpdater) Enroll(inst Installed) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.entries[inst.ID] = &updateEntry{
		inst:   inst,
		ladder: NewRecoveryLadder(u.cfg.Policy),
	}
}

// Forget drops a transport from the updater (it was removed or disabled).
func (u *AutoUpdater) Forget(id string) {
	u.mu.Lock()
	delete(u.entries, id)
	u.mu.Unlock()
}

// OnConnected tells the ladder for id that its transport is up.
func (u *AutoUpdater) OnConnected(id string) {
	u.mu.Lock()
	e := u.entries[id]
	if e != nil {
		e.ladder.OnConnected(u.now())
	}
	u.mu.Unlock()
}

// OnFailed tells the ladder for id that a connect attempt failed and acts on
// whatever step that earns (in the background, so the caller's supervision
// goroutine is never blocked on a fetch).
func (u *AutoUpdater) OnFailed(id string) {
	u.mu.Lock()
	e := u.entries[id]
	var action RecoveryAction
	if e != nil {
		action = e.ladder.OnFailed(u.now())
	}
	u.mu.Unlock()
	if action != RecoverNone {
		u.dispatch(id, action)
	}
}

// dispatch queues an action for id and starts the per-id worker if idle. The
// worker runs off the caller's goroutine so the supervision loop that calls
// OnFailed is never blocked on a fetch.
func (u *AutoUpdater) dispatch(id string, action RecoveryAction) {
	u.mu.Lock()
	u.pending[id] = action
	if u.running[id] {
		u.mu.Unlock()
		return
	}
	u.running[id] = true
	u.mu.Unlock()
	utils.SafeGo("script.autoupdate."+id, func() { u.work(id) })
}

// work drains id's pending action one at a time, in the order they were
// requested (monotonically up the ladder), until none is left.
func (u *AutoUpdater) work(id string) {
	for {
		u.mu.Lock()
		action, ok := u.pending[id]
		delete(u.pending, id)
		if !ok {
			u.running[id] = false
			u.mu.Unlock()
			return
		}
		u.mu.Unlock()

		switch action {
		case RecoverRecheck:
			rep := u.checkAndMaybeApply(context.Background(), id)
			utils.Infof("[SCRIPT-UPDATE] %q recovery=recheck status=%s code=%s", id, rep.Status, rep.Code)
		case RecoverRollback:
			rep := u.rollback(id)
			utils.Infof("[SCRIPT-UPDATE] %q recovery=rollback status=%s code=%s", id, rep.Status, rep.Code)
		case RecoverFallback:
			utils.Infof("[SCRIPT-UPDATE] %q recovery=fallback (scripted path exhausted)", id)
			if u.cfg.Fallback != nil {
				u.cfg.Fallback(id)
			}
		}
	}
}

// CheckAll runs one pass over every enrolled transport: check for an update
// and, when Auto is on, install the ones the core says need no question. This
// is the daily stay-current pass; the ladder handles out-of-band breakage.
func (u *AutoUpdater) CheckAll(ctx context.Context) {
	for _, id := range u.ids() {
		select {
		case <-ctx.Done():
			return
		default:
		}
		rep := u.checkAndMaybeApply(ctx, id)
		switch {
		case rep.Status == UpdateInstalled:
			utils.Infof("[SCRIPT-UPDATE] %q auto-applied -> %s", id, rep.Latest)
		case rep.Status == UpdateAvailable:
			utils.Infof("[SCRIPT-UPDATE] %q update available %s->%s (not auto: official=%v autoOk=%v code=%s)",
				id, rep.Current, rep.Latest, rep.Official, rep.AutoOK, rep.Code)
		case rep.Status == UpdateError:
			if utils.Throttled("script.update.check."+id, 30*time.Minute) {
				utils.Infof("[SCRIPT-UPDATE] %q check error code=%s", id, rep.Code)
			}
		}
	}
}

// checkAndMaybeApply checks id and installs the update when, and only when, it
// is an auto-OK one (official key, same wire, same key) and Auto is on - the
// single rule for an unattended change, shared by the daily pass and the
// ladder's recheck step so both behave identically. An update that is not
// auto-OK is reported, not installed: a wire break needs both sides, a key
// change is a new trust decision, a third-party transport is the operator's
// call.
func (u *AutoUpdater) checkAndMaybeApply(ctx context.Context, id string) UpdateReport {
	rep := u.check(ctx, id)
	if rep.Status == UpdateAvailable && u.cfg.Auto && rep.AutoOK {
		return u.apply(ctx, id, false)
	}
	return rep
}

// Run checks everything now and then every `every` until ctx is cancelled.
func (u *AutoUpdater) Run(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = 24 * time.Hour
	}
	u.CheckAll(ctx)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			u.CheckAll(ctx)
		}
	}
}

// Watch drives one transport's recovery ladder from a liveness probe until
// ctx is done. It polls up() every interval and feeds the ladder both edges
// and duration:
//   - an up edge (down->up) is a fresh OnConnected;
//   - a down edge (up->down) is one OnFailed;
//   - each further `grace` of continuous down is another OnFailed, so a lasting
//     outage escalates steadily instead of counting as a single failure.
//
// Flapping and sustained outages both reach the ladder this way, while the
// ladder's own StableFor rule keeps a transport that only ever connects for an
// instant from resetting (see RecoveryLadder). Run one Watch per enrolled
// transport; `up` is typically the transport's IsConnected.
func (u *AutoUpdater) Watch(ctx context.Context, id string, up func() bool, interval, grace time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if grace <= 0 {
		grace = 30 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	wasUp := false
	downSince := u.now() // treat the pre-start state as down since now
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := u.now()
		if up() {
			if !wasUp {
				u.OnConnected(id)
			}
			wasUp = true
			downSince = time.Time{}
			continue
		}
		// Down.
		if wasUp {
			u.OnFailed(id) // up -> down edge
			downSince = now
			wasUp = false
			continue
		}
		if downSince.IsZero() {
			downSince = now
		} else if now.Sub(downSince) >= grace {
			u.OnFailed(id) // still down a whole grace later
			downSince = now
		}
	}
}

func (u *AutoUpdater) ids() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]string, 0, len(u.entries))
	for id := range u.entries {
		out = append(out, id)
	}
	return out
}

func (u *AutoUpdater) installed(id string) (Installed, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	e := u.entries[id]
	if e == nil {
		return Installed{}, false
	}
	return e.inst, true
}

func (u *AutoUpdater) check(ctx context.Context, id string) UpdateReport {
	inst, ok := u.installed(id)
	if !ok {
		return UpdateReport{Status: UpdateError, Code: "gone"}
	}
	return CheckUpdate(ctx, inst, u.cfg.Channel, u.fetch)
}

func (u *AutoUpdater) apply(ctx context.Context, id string, allowWireBreak bool) UpdateReport {
	inst, ok := u.installed(id)
	if !ok {
		return UpdateReport{Status: UpdateError, Code: "gone"}
	}
	rep := ApplyUpdate(ctx, inst, u.cfg.Channel, u.cfg.Dir, allowWireBreak, u.fetch)
	if rep.Status == UpdateInstalled {
		u.onPackageChanged(id, rep.Latest, rep.Wire, rep.NewKey)
	}
	return rep
}

func (u *AutoUpdater) rollback(id string) UpdateReport {
	inst, ok := u.installed(id)
	if !ok {
		return UpdateReport{Status: UpdateError, Code: "gone"}
	}
	rep := RollbackPackage(u.cfg.Dir, inst.FileName(), inst.PubkeyHex)
	if rep.Status == UpdateInstalled {
		u.onPackageChanged(id, rep.Latest, rep.Wire, "")
	}
	return rep
}

// onPackageChanged records the new version (and key, on a rotation) so the
// next check compares against what is now on disk, then asks the owner to
// reload the live link from it.
func (u *AutoUpdater) onPackageChanged(id, version string, wire int, newKey string) {
	u.mu.Lock()
	if e := u.entries[id]; e != nil {
		e.inst.Version = version
		if wire > 0 {
			e.inst.Wire = wire
		}
		if newKey != "" {
			e.inst.PubkeyHex = newKey
		}
	}
	u.mu.Unlock()
	if u.cfg.Reload != nil {
		u.cfg.Reload(id)
	}
}
