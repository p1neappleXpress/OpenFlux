package script

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// install publishes a manifest, writes it as demo.flux in dir (moving any
// current one to demo.flux.prev), and returns the package bytes.
func (r *updateRig) install(dir string, m PackageManifest) []byte {
	b := r.publish(m, r.priv)
	if err := InstallPackage(dir, "demo.flux", b); err != nil {
		r.t.Fatal(err)
	}
	return b
}

func demoInstalled(r *updateRig, version string, wire int) Installed {
	return Installed{ID: "demo", File: "demo.flux", Version: version, Wire: wire, PubkeyHex: r.pubHex(), Update: []string{idxURL}}
}

func waitFor(t *testing.T, ch <-chan string, want string) {
	t.Helper()
	select {
	case got := <-ch:
		if got != want {
			t.Fatalf("callback id = %q, want %q", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %q", want)
	}
}

func version(t *testing.T, r *updateRig, path string) string {
	t.Helper()
	p, err := LoadSignedPackage(path, r.pub)
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	return p.Manifest.Version
}

func TestAutoUpdaterRecoveryRollsBackAndReloads(t *testing.T) {
	r := newRig(t)
	dir := t.TempDir()
	r.install(dir, PackageManifest{ID: "demo", Name: "demo", Version: "1.0.0"}) // becomes .prev
	r.install(dir, PackageManifest{ID: "demo", Name: "demo", Version: "1.1.0"}) // current
	// The index says 1.1.0 is current, so the recheck step is a harmless no-op
	// and the ladder walks on to a rollback.
	r.setIndex(idxURL, UpdateIndex{Format: 1, ID: "demo", Channels: map[string]IndexChannel{
		"stable": {Version: "1.1.0", URL: "demo-1.1.0.flux", SHA256: sha(r.body[idxURL])},
	}})

	reloaded := make(chan string, 4)
	u := NewAutoUpdater(AutoUpdaterConfig{
		Dir:    dir,
		Auto:   true,
		Policy: RecoveryPolicy{RecheckAfter: 2, RollbackAfter: 3, FallbackAfter: 9, Window: time.Hour, StableFor: time.Hour},
		Fetch:  r.get,
		Reload: func(id string) { reloaded <- id },
	})
	u.Enroll(demoInstalled(r, "1.1.0", 1))

	for i := 0; i < 3; i++ { // 1: none, 2: recheck (no-op), 3: rollback
		u.OnFailed("demo")
	}
	waitFor(t, reloaded, "demo")
	if v := version(t, r, filepath.Join(dir, "demo.flux")); v != "1.0.0" {
		t.Fatalf("after rollback current = %s, want 1.0.0", v)
	}
}

func TestAutoUpdaterRecoveryFallsBackWhenExhausted(t *testing.T) {
	r := newRig(t)
	dir := t.TempDir()
	r.install(dir, PackageManifest{ID: "demo", Name: "demo", Version: "1.0.0"})

	fellBack := make(chan string, 4)
	u := NewAutoUpdater(AutoUpdaterConfig{
		Dir:      dir,
		Auto:     true,
		Policy:   RecoveryPolicy{RecheckAfter: 1, RollbackAfter: 2, FallbackAfter: 3, Window: time.Hour, StableFor: time.Hour},
		Fetch:    r.get, // no index served: recheck/rollback are harmless failures
		Fallback: func(id string) { fellBack <- id },
	})
	u.Enroll(demoInstalled(r, "1.0.0", 1))

	for i := 0; i < 3; i++ {
		u.OnFailed("demo")
	}
	waitFor(t, fellBack, "demo")
}

func TestAutoUpdaterConnectedResetsSoNoFallback(t *testing.T) {
	r := newRig(t)
	dir := t.TempDir()
	r.install(dir, PackageManifest{ID: "demo", Name: "demo", Version: "1.0.0"})

	fellBack := make(chan string, 4)
	u := NewAutoUpdater(AutoUpdaterConfig{
		Dir:      dir,
		Auto:     true,
		Policy:   RecoveryPolicy{RecheckAfter: 2, RollbackAfter: 3, FallbackAfter: 4, Window: time.Hour, StableFor: 20 * time.Millisecond},
		Fetch:    r.get,
		Fallback: func(id string) { fellBack <- id },
	})
	u.Enroll(demoInstalled(r, "1.0.0", 1))

	// Fail a few times, then a connect that holds past StableFor; the ladder
	// must reset, so no later burst of two reaches fallback.
	u.OnFailed("demo")
	u.OnFailed("demo") // a recheck fires, harmless
	u.OnConnected("demo")
	time.Sleep(50 * time.Millisecond) // > StableFor: the connection counts as recovered
	u.OnFailed("demo")
	u.OnFailed("demo")
	select {
	case id := <-fellBack:
		t.Fatalf("fell back for %q after a recovered connection reset the ladder", id)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestAutoUpdaterDailyLeavesNonAutoOKAlone(t *testing.T) {
	r := newRig(t)
	dir := t.TempDir()
	r.install(dir, PackageManifest{ID: "demo", Name: "demo", Version: "1.0.0"})
	newer := r.publish(PackageManifest{ID: "demo", Name: "demo", Version: "1.1.0"}, r.priv)
	r.body["https://example.test/demo/demo-1.1.0.flux"] = newer
	r.setIndex(idxURL, UpdateIndex{Format: 1, ID: "demo", Channels: map[string]IndexChannel{
		"stable": {Version: "1.1.0", URL: "demo-1.1.0.flux", SHA256: sha(newer)},
	}})

	u := NewAutoUpdater(AutoUpdaterConfig{Dir: dir, Auto: true, Fetch: r.get})
	u.Enroll(demoInstalled(r, "1.0.0", 1))

	// The update is available but the key is not an official one, so autoOk is
	// false: the daily pass must not install it on its own.
	u.CheckAll(context.Background())
	if v := version(t, r, filepath.Join(dir, "demo.flux")); v != "1.0.0" {
		t.Fatalf("a non-auto-OK update was installed unattended: now %s", v)
	}
	if _, err := LoadSignedPackage(filepath.Join(dir, "demo.flux.prev"), r.pub); err == nil {
		t.Fatal("a .prev exists, so an install happened; it should not have")
	}
}

func TestAutoUpdaterWatchSustainedDownEscalates(t *testing.T) {
	r := newRig(t)
	dir := t.TempDir()
	r.install(dir, PackageManifest{ID: "demo", Name: "demo", Version: "1.0.0"})

	fellBack := make(chan string, 4)
	u := NewAutoUpdater(AutoUpdaterConfig{
		Dir:      dir,
		Auto:     true,
		Policy:   RecoveryPolicy{RecheckAfter: 1, RollbackAfter: 2, FallbackAfter: 3, Window: time.Hour, StableFor: time.Hour},
		Fetch:    r.get, // no index served: recheck/rollback are harmless no-ops
		Fallback: func(id string) { fellBack <- id },
	})
	u.Enroll(demoInstalled(r, "1.0.0", 1))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Transport never comes up: Watch should raise failures every grace and
	// walk the ladder to fallback.
	go u.Watch(ctx, "demo", func() bool { return false }, 5*time.Millisecond, 8*time.Millisecond)
	waitFor(t, fellBack, "demo")
}

func TestAutoUpdaterWatchHealthyStaysCalm(t *testing.T) {
	r := newRig(t)
	dir := t.TempDir()
	r.install(dir, PackageManifest{ID: "demo", Name: "demo", Version: "1.0.0"})

	fellBack := make(chan string, 4)
	u := NewAutoUpdater(AutoUpdaterConfig{
		Dir:      dir,
		Auto:     true,
		Policy:   RecoveryPolicy{RecheckAfter: 1, RollbackAfter: 2, FallbackAfter: 3, Window: time.Hour, StableFor: time.Hour},
		Fetch:    r.get,
		Fallback: func(id string) { fellBack <- id },
	})
	u.Enroll(demoInstalled(r, "1.0.0", 1))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Transport is up the whole time: no failures, so no escalation ever.
	go u.Watch(ctx, "demo", func() bool { return true }, 5*time.Millisecond, 8*time.Millisecond)
	select {
	case id := <-fellBack:
		t.Fatalf("a healthy transport must not escalate; fell back for %q", id)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestAutoUpdaterSkipsPlainJSAndMissingSource(t *testing.T) {
	// A transport with no update source is simply not updatable; Installed()
	// returns ok=false and it is never enrolled. Here we check the record
	// guard that drives that decision directly.
	st := &ScriptTransport{} // no package, no key
	if _, ok := st.Installed(); ok {
		t.Fatal("a transport with no package must not produce an Installed record")
	}
}
