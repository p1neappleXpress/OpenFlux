package script

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"1.2.3", "1.2.3", 0},
		{"1.2.4", "1.2.3", 1},
		{"1.10.0", "1.9.9", 1},
		{"v2.0.0", "1.99.99", 1},
		{"1.0.0-beta.1", "1.0.0", -1},
		{"1.0.0-beta.2", "1.0.0-beta.10", -1},
		{"1.0.0-alpha", "1.0.0-alpha.1", -1},
		{"1.0.0+build5", "1.0.0", 0},
		{"junk", "0.0.1", -1},
		{"", "", 0},
	} {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// updateRig is an author with a key, a set of published packages and an
// index, served through a fake Fetcher.
type updateRig struct {
	t    *testing.T
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
	body map[string][]byte // url -> answer
	hits map[string]int
}

func newRig(t *testing.T) *updateRig {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &updateRig{t: t, pub: pub, priv: priv, body: map[string][]byte{}, hits: map[string]int{}}
}

func (r *updateRig) pubHex() string { return hex.EncodeToString(r.pub) }

func (r *updateRig) get(_ context.Context, u string, _ int64) ([]byte, error) {
	r.hits[u]++
	if b, ok := r.body[u]; ok {
		return b, nil
	}
	return nil, fmt.Errorf("HTTP 404")
}

const rigScript = `var Transport = { info: function() { return { name: "demo", version: "1.0.0" }; } };`

// publish builds a signed package and returns its bytes.
func (r *updateRig) publish(m PackageManifest, key ed25519.PrivateKey) []byte {
	path := filepath.Join(r.t.TempDir(), "p.flux")
	if err := WritePackage(path, m, []byte(rigScript), nil, key, true); err != nil {
		r.t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		r.t.Fatal(err)
	}
	return b
}

func sha(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func (r *updateRig) setIndex(url string, idx UpdateIndex) {
	b, _ := json.Marshal(idx)
	r.body[url] = b
}

const idxURL = "https://example.test/demo/update.json"

func inst(r *updateRig, version string) Installed {
	return Installed{ID: "demo", Version: version, PubkeyHex: r.pubHex(), Update: []string{idxURL}}
}

func TestCheckUpdateStates(t *testing.T) {
	r := newRig(t)
	pkg := r.publish(PackageManifest{ID: "demo", Name: "demo", Version: "1.1.0"}, r.priv)
	r.body["https://example.test/demo/demo-1.1.0.flux"] = pkg
	r.setIndex(idxURL, UpdateIndex{Format: 1, ID: "demo", Channels: map[string]IndexChannel{
		"stable": {Version: "1.1.0", URL: "demo-1.1.0.flux", SHA256: sha(pkg), Notes: "fixes"},
	}})

	rep := CheckUpdate(context.Background(), inst(r, "1.0.0"), "stable", r.get)
	if rep.Status != UpdateAvailable || rep.Latest != "1.1.0" || rep.Notes != "fixes" || rep.WireBreak {
		t.Fatalf("available: %+v", rep)
	}
	if rep.AutoOK {
		t.Fatal("a third-party transport must not be AutoOK")
	}
	if got := CheckUpdate(context.Background(), inst(r, "1.1.0"), "stable", r.get); got.Status != UpdateUpToDate {
		t.Fatalf("same version: %+v", got)
	}
	if got := CheckUpdate(context.Background(), inst(r, "1.2.0"), "stable", r.get); got.Status != UpdateUpToDate {
		t.Fatalf("an index older than the installed one must never offer a downgrade: %+v", got)
	}
	// An unknown channel falls back to stable.
	if got := CheckUpdate(context.Background(), inst(r, "1.0.0"), "nightly", r.get); got.Status != UpdateAvailable {
		t.Fatalf("channel fallback: %+v", got)
	}
}

func TestCheckUpdateOfficialIsAutoOK(t *testing.T) {
	r := newRig(t)
	r.setIndex(idxURL, UpdateIndex{Format: 1, ID: "demo", Channels: map[string]IndexChannel{"stable": {Version: "1.1.0", URL: "x.flux"}}})
	i := inst(r, "1.0.0")
	i.PubkeyHex = OfficialKeyHex
	rep := CheckUpdate(context.Background(), i, "stable", r.get)
	if !rep.AutoOK || !rep.Official {
		t.Fatalf("official, same wire, same key: %+v", rep)
	}
}

func TestCheckUpdateBlocksAndErrors(t *testing.T) {
	r := newRig(t)
	set := func(ch IndexChannel) {
		ch.URL = "x.flux"
		r.setIndex(idxURL, UpdateIndex{Format: 1, ID: "demo", Channels: map[string]IndexChannel{"stable": ch}})
	}
	check := func() UpdateReport { return CheckUpdate(context.Background(), inst(r, "1.0.0"), "stable", r.get) }

	set(IndexChannel{Version: "2.0.0", Wire: 2})
	if rep := check(); rep.Status != UpdateAvailable || !rep.WireBreak || rep.Code != CodeWireBreak || rep.AutoOK {
		t.Fatalf("wire break: %+v", rep)
	}
	set(IndexChannel{Version: "1.1.0", API: APIVersion + 1})
	if rep := check(); rep.Status != UpdateBlocked || rep.Code != CodeNeedsNewerApp {
		t.Fatalf("api too new: %+v", rep)
	}
	// A key that differs from the signer's: not "00..." when the signer's own key happens to start with it (1 in 256).
	other := "00"
	if r.pubHex()[:2] == "00" {
		other = "01"
	}
	set(IndexChannel{Version: "1.1.0", PublicKey: other + r.pubHex()[2:]})
	if rep := check(); rep.Status != UpdateBlocked || rep.Code != CodeKeyChanged {
		t.Fatalf("announced key change: %+v", rep)
	}
	set(IndexChannel{Version: "not-a-version"})
	if rep := check(); rep.Code != CodeBadIndex {
		t.Fatalf("bad version: %+v", rep)
	}

	// Wrong id, wrong format, no source, insecure source, dead source.
	r.setIndex(idxURL, UpdateIndex{Format: 1, ID: "other", Channels: map[string]IndexChannel{"stable": {Version: "9.9.9"}}})
	if rep := check(); rep.Code != CodeIDMismatch {
		t.Fatalf("id mismatch: %+v", rep)
	}
	r.setIndex(idxURL, UpdateIndex{Format: 7, ID: "demo"})
	if rep := check(); rep.Code != CodeBadIndex {
		t.Fatalf("format: %+v", rep)
	}
	i := inst(r, "1.0.0")
	i.Update = nil
	if rep := CheckUpdate(context.Background(), i, "stable", r.get); rep.Code != CodeNoSource {
		t.Fatalf("no source: %+v", rep)
	}
	i.Update = []string{"http://example.test/update.json"}
	if rep := CheckUpdate(context.Background(), i, "stable", r.get); rep.Code != CodeInsecureURL {
		t.Fatalf("insecure: %+v", rep)
	}
	i.Update = []string{"https://gone.test/update.json"}
	if rep := CheckUpdate(context.Background(), i, "stable", r.get); rep.Code != CodeFetchFailed {
		t.Fatalf("dead: %+v", rep)
	}
}

func TestCheckUpdateUsesMirrorWhenFirstIsDown(t *testing.T) {
	r := newRig(t)
	r.setIndex("https://mirror.test/update.json", UpdateIndex{Format: 1, ID: "demo", Channels: map[string]IndexChannel{"stable": {Version: "1.1.0", URL: "x.flux"}}})
	i := inst(r, "1.0.0")
	i.Update = []string{"https://dead.test/update.json", "https://mirror.test/update.json"}
	if rep := CheckUpdate(context.Background(), i, "stable", r.get); rep.Status != UpdateAvailable {
		t.Fatalf("%+v", rep)
	}
}

func TestApplyUpdateInstallsAndKeepsPrevious(t *testing.T) {
	r := newRig(t)
	dir := t.TempDir()
	old := r.publish(PackageManifest{ID: "demo", Name: "demo", Version: "1.0.0"}, r.priv)
	if err := InstallPackage(dir, "demo.flux", old); err != nil {
		t.Fatal(err)
	}
	pkg := r.publish(PackageManifest{ID: "demo", Name: "demo", Version: "1.1.0"}, r.priv)
	r.body["https://example.test/demo/demo-1.1.0.flux"] = pkg
	r.setIndex(idxURL, UpdateIndex{Format: 1, ID: "demo", Channels: map[string]IndexChannel{
		"stable": {Version: "1.1.0", URL: "demo-1.1.0.flux", SHA256: sha(pkg)},
	}})

	rep := ApplyUpdate(context.Background(), inst(r, "1.0.0"), "stable", dir, false, r.get)
	if rep.Status != UpdateInstalled || rep.Code != "" {
		t.Fatalf("%+v", rep)
	}
	p, err := LoadSignedPackage(filepath.Join(dir, "demo.flux"), r.pub)
	if err != nil || p.Manifest.Version != "1.1.0" {
		t.Fatalf("current after update: %v %+v", err, p)
	}
	prev, err := LoadSignedPackage(filepath.Join(dir, "demo.flux.prev"), r.pub)
	if err != nil || prev.Manifest.Version != "1.0.0" {
		t.Fatalf("previous after update: %v", err)
	}

	// Roll back, then forward again: the swap keeps both.
	if rb := RollbackPackage(dir, "demo.flux", r.pubHex()); rb.Status != UpdateInstalled || rb.Latest != "1.0.0" {
		t.Fatalf("rollback: %+v", rb)
	}
	if p, _ := LoadSignedPackage(filepath.Join(dir, "demo.flux"), r.pub); p == nil || p.Manifest.Version != "1.0.0" {
		t.Fatal("rollback did not restore 1.0.0")
	}
	if rb := RollbackPackage(dir, "demo.flux", r.pubHex()); rb.Latest != "1.1.0" {
		t.Fatalf("roll forward: %+v", rb)
	}
}

func TestApplyUpdateRefusals(t *testing.T) {
	r := newRig(t)
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	good := PackageManifest{ID: "demo", Name: "demo", Version: "1.1.0"}

	cases := []struct {
		name string
		pkg  []byte
		sha  string // "" = of pkg
		ch   IndexChannel
		code string
	}{
		{"signed by another key", r.publish(good, otherPriv), "", IndexChannel{Version: "1.1.0"}, CodeBadSignature},
		{"hash differs from the index", r.publish(good, r.priv), "00", IndexChannel{Version: "1.1.0"}, CodeBadHash},
		{"package is another transport", r.publish(PackageManifest{ID: "evil", Name: "evil", Version: "1.1.0"}, r.priv), "", IndexChannel{Version: "1.1.0"}, CodeIDMismatch},
		{"package version is not the announced one", r.publish(PackageManifest{ID: "demo", Name: "demo", Version: "1.0.5"}, r.priv), "", IndexChannel{Version: "1.1.0"}, CodeVersionMismatch},
		{"package wire is not the announced one", r.publish(PackageManifest{ID: "demo", Name: "demo", Version: "1.1.0", Wire: 2}, r.priv), "", IndexChannel{Version: "1.1.0"}, CodeVersionMismatch},
		{"not a package", []byte("PK nope"), "", IndexChannel{Version: "1.1.0"}, CodeBadPackage},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			c.ch.URL = "demo.flux"
			c.ch.SHA256 = c.sha
			if c.ch.SHA256 == "" {
				c.ch.SHA256 = sha(c.pkg)
			}
			r.body["https://example.test/demo/demo.flux"] = c.pkg
			r.setIndex(idxURL, UpdateIndex{Format: 1, ID: "demo", Channels: map[string]IndexChannel{"stable": c.ch}})
			rep := ApplyUpdate(context.Background(), inst(r, "1.0.0"), "stable", dir, false, r.get)
			if rep.Status != UpdateError || rep.Code != c.code {
				t.Fatalf("got %+v, want error %s", rep, c.code)
			}
			if _, err := os.Stat(filepath.Join(dir, "demo.flux")); err == nil {
				t.Fatal("a refused update must leave nothing installed")
			}
		})
	}
}

func TestApplyUpdateWireBreakNeedsConsent(t *testing.T) {
	r := newRig(t)
	pkg := r.publish(PackageManifest{ID: "demo", Name: "demo", Version: "2.0.0", Wire: 2}, r.priv)
	r.body["https://example.test/demo/d.flux"] = pkg
	r.setIndex(idxURL, UpdateIndex{Format: 1, ID: "demo", Channels: map[string]IndexChannel{
		"stable": {Version: "2.0.0", Wire: 2, URL: "d.flux", SHA256: sha(pkg)},
	}})
	dir := t.TempDir()
	if rep := ApplyUpdate(context.Background(), inst(r, "1.0.0"), "stable", dir, false, r.get); rep.Status != UpdateBlocked || rep.Code != CodeWireBreak {
		t.Fatalf("without consent: %+v", rep)
	}
	if r.hits["https://example.test/demo/d.flux"] != 0 {
		t.Fatal("a blocked update must not even download the package")
	}
	if rep := ApplyUpdate(context.Background(), inst(r, "1.0.0"), "stable", dir, true, r.get); rep.Status != UpdateInstalled {
		t.Fatalf("with consent: %+v", rep)
	}
}

func TestRollbackWithoutPrevious(t *testing.T) {
	r := newRig(t)
	if rep := RollbackPackage(t.TempDir(), "demo.flux", r.pubHex()); rep.Code != CodeNoPrevious {
		t.Fatalf("%+v", rep)
	}
}

func TestRollbackRefusesTamperedPrevious(t *testing.T) {
	r := newRig(t)
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "demo.flux.prev"), r.publish(PackageManifest{ID: "demo", Name: "demo", Version: "1.0.0"}, r.priv), 0o644)
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	_ = os.WriteFile(filepath.Join(dir, "demo.flux.prev"), r.publish(PackageManifest{ID: "demo", Name: "demo", Version: "1.0.0"}, otherPriv), 0o644)
	if rep := RollbackPackage(dir, "demo.flux", r.pubHex()); rep.Code != CodeBadSignature {
		t.Fatalf("%+v", rep)
	}
}

func TestManifestDefaults(t *testing.T) {
	m := PackageManifest{Name: "My Transport"}
	if m.EffectiveID() != "my-transport" || m.EffectiveWire() != 1 || m.EffectiveAPI() != 1 {
		t.Fatalf("defaults: %q %d %d", m.EffectiveID(), m.EffectiveWire(), m.EffectiveAPI())
	}
}

// A rotation of the project's own keys: official transports move to the new
// key, third-party ones never change key through an update.
func TestOfficialKeyRotation(t *testing.T) {
	a, b, c := newRig(t), newRig(t), newRig(t)
	saved := officialKeys
	officialKeys = []string{a.pubHex(), b.pubHex()}
	defer func() { officialKeys = saved }()

	publish := func(signer *updateRig, version string) (url string, idx UpdateIndex) {
		pkg := signer.publish(PackageManifest{ID: "demo", Name: "demo", Version: version}, signer.priv)
		a.body["https://example.test/demo/rot.flux"] = pkg
		return "rot.flux", UpdateIndex{Format: 1, ID: "demo", Channels: map[string]IndexChannel{
			"stable": {Version: version, URL: "rot.flux", SHA256: sha(pkg)},
		}}
	}
	installed := func(pin *updateRig) Installed {
		return Installed{ID: "demo", Version: "1.0.0", PubkeyHex: pin.pubHex(), Update: []string{idxURL}}
	}

	// official A -> package signed by official B: installed, re-pinned, no question asked
	_, idx := publish(b, "1.1.0")
	a.setIndex(idxURL, idx)
	dir := t.TempDir()
	rep := ApplyUpdate(context.Background(), installed(a), "stable", dir, false, a.get)
	if rep.Status != UpdateInstalled || rep.NewKey != b.pubHex() || !rep.Official {
		t.Fatalf("rotation A->B: %+v", rep)
	}
	if rep2 := CheckUpdate(context.Background(), installed(a), "stable", a.get); !rep2.AutoOK {
		t.Fatalf("a rotation between official keys should need no question: %+v", rep2)
	}

	// signed by a key that is not the project's: refused
	_, idx = publish(c, "1.2.0")
	a.setIndex(idxURL, idx)
	if rep := ApplyUpdate(context.Background(), installed(a), "stable", t.TempDir(), false, a.get); rep.Status != UpdateError || rep.Code != CodeBadSignature {
		t.Fatalf("foreign key for an official install: %+v", rep)
	}

	// a third-party install never hops to another key, even an official one
	_, idx = publish(b, "1.3.0")
	a.setIndex(idxURL, idx)
	if rep := ApplyUpdate(context.Background(), installed(c), "stable", t.TempDir(), false, a.get); rep.Code != CodeBadSignature {
		t.Fatalf("a third-party install must stay on its pinned key: %+v", rep)
	}

	// the index announcing an official key for an official install is not a key change; a foreign one is
	_, idx = publish(b, "1.4.0")
	ch := idx.Channels["stable"]
	ch.PublicKey = b.pubHex()
	idx.Channels["stable"] = ch
	a.setIndex(idxURL, idx)
	if rep := CheckUpdate(context.Background(), installed(a), "stable", a.get); rep.Code == CodeKeyChanged {
		t.Fatalf("A->B announced: %+v", rep)
	}
	ch.PublicKey = c.pubHex()
	idx.Channels["stable"] = ch
	a.setIndex(idxURL, idx)
	if rep := CheckUpdate(context.Background(), installed(a), "stable", a.get); rep.Code != CodeKeyChanged {
		t.Fatalf("a foreign announced key must be blocked: %+v", rep)
	}
}

// After a rotation the previous version is still signed by the old official key.
func TestRollbackAfterOfficialRotation(t *testing.T) {
	a, b := newRig(t), newRig(t)
	saved := officialKeys
	officialKeys = []string{a.pubHex(), b.pubHex()}
	defer func() { officialKeys = saved }()

	dir := t.TempDir()
	_ = InstallPackage(dir, "demo.flux", a.publish(PackageManifest{ID: "demo", Name: "demo", Version: "1.0.0"}, a.priv))
	_ = InstallPackage(dir, "demo.flux", b.publish(PackageManifest{ID: "demo", Name: "demo", Version: "1.1.0"}, b.priv))
	// pinned to B now, the previous (1.0.0) is A's
	if rep := RollbackPackage(dir, "demo.flux", b.pubHex()); rep.Status != UpdateInstalled || rep.Latest != "1.0.0" {
		t.Fatalf("%+v", rep)
	}
}
