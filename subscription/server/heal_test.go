package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/p1neappleXpress/OpenFlux/provision"
	"github.com/p1neappleXpress/OpenFlux/share"
)

func TestAutoNodeConfigRules(t *testing.T) {
	good := func() *Config {
		return &Config{
			PublicHost: "h", AdminToken: strings.Repeat("x", 24),
			Pools: []Pool{{ID: "a", Name: "A", Channel: "c", AutoNode: true,
				Transports: []provision.ChannelTransport{{Type: "cupsonline", URL: ""}}}},
			Plans: []Plan{{ID: "p", Name: "P", Days: 1, Devices: 1}},
		}
	}
	if err := good().Check(); err != nil {
		t.Fatal(err)
	}
	auto := good()
	if err := auto.Check(); err != nil {
		t.Fatal(err)
	}
	if got := auto.NodeScript; got != "/opt/openflux-node/node-install.sh" {
		t.Fatalf("NodeScript default %q", got)
	}
	var emptyCups = []provision.ChannelTransport{{Type: "cupsonline", URL: ""}}
	for name, mutate := range map[string]func(*Config){
		"underscore channel":  func(c *Config) { c.Pools[0].Channel = "a_b" },
		"bad transport":       func(c *Config) { c.Pools[0].Transports = []provision.ChannelTransport{{Type: "vyandex"}} },
		"duplicate transport": func(c *Config) {
			c.Pools[0].Transports = append(append(emptyCups, emptyCups...), emptyCups...)
		},
		"yandex empty url":       func(c *Config) { c.Pools[0].Transports = []provision.ChannelTransport{{Type: "yandex"}} },
		"cups without autoNode":  func(c *Config) { c.Pools[0].AutoNode = false },
		"bad mode":               func(c *Config) { c.Pools[0].Mode = "l9" },
	} {
		c := good()
		mutate(c)
		if err := c.Check(); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	// No autoNode: the old pools stay valid (an empty cups transport nowhere).
	plain := &Config{
		PublicHost: "h", AdminToken: strings.Repeat("x", 24),
		Pools: []Pool{{ID: "a", Name: "A", Channel: "c"}},
		Plans: []Plan{{ID: "p", Name: "P", Days: 1, Devices: 1}},
	}
	if err := plain.Check(); err != nil {
		t.Fatal(err)
	}
}

// fakeProvisioner is what the tests hand the server instead of
// node-install.sh: it records calls and, when not failing, writes the channel
// like writeChannel does so links come up.
type fakeProvisioner struct {
	mu      sync.Mutex
	calls   int
	channel string
	nodes   string
	t       *testing.T
	fail    bool
	started chan struct{} // closed on first Ensure, to run the single-flight test
	block   chan struct{} // when set, Ensure waits here before writing
}

func (f *fakeProvisioner) Ensure(_ context.Context, r nodeRequest) error {
	f.mu.Lock()
	f.calls++
	f.channel = r.Channel
	started, block, fail, nodes := f.started, f.block, f.fail, f.nodes
	f.mu.Unlock()
	if started != nil {
		f.mu.Lock()
		select {
		case <-started:
		default:
			close(started)
		}
		f.mu.Unlock()
	}
	if block != nil {
		<-block
	}
	if fail {
		return errors.New("не смог")
	}
	writeChannel(f.t, nodes, r.Channel, []string{mailruDoc}, "35123")
	return nil
}

func (f *fakeProvisioner) count() int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

func (e *env) waitForPool(t *testing.T, token string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, doc := e.call("GET", "/api/v1/subscription", token, nil)
		if len(doc.list("profiles")) == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("profiles never reached %d: %v", n, doc)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAutoNodeProvisionsMissingChannel(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.Pools[0].AutoNode = true })
	os.Remove(filepath.Join(e.nodes, "fork-all", "node.conf"))
	fake := &fakeProvisioner{nodes: e.nodes, t: t, started: make(chan struct{})}
	e.prov = fake
	e.start(e.srv.cfg)
	token, _ := e.register("ivan", "a")

	_, doc := e.call("GET", "/api/v1/subscription", token, nil)
	if doc["profiles"] == nil {
		t.Fatalf(`while the channel is being made the answer must be "profiles":[], not null: %v`, doc)
	}
	select {
	case <-fake.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the pool did not ask for its channel")
	}
	e.waitForPool(t, token, 2)
	if fake.channel != "fork-all" {
		t.Fatalf("channel %q", fake.channel)
	}
}

func TestAutoNodeSingleFlight(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.Pools[0].AutoNode = true })
	os.Remove(filepath.Join(e.nodes, "fork-all", "node.conf"))
	fake := &fakeProvisioner{nodes: e.nodes, t: t, started: make(chan struct{}), block: make(chan struct{})}
	e.prov = fake
	e.start(e.srv.cfg)
	token, _ := e.register("ivan", "a")

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.call("GET", "/api/v1/subscription", token, nil)
		}()
	}
	select {
	case <-fake.started:
	case <-time.After(5 * time.Second):
		t.Fatal("ensure did not start")
	}
	close(fake.block)
	wg.Wait()
	if c := fake.count(); c != 1 {
		t.Fatalf("calls %d, want one creation for two requests", c)
	}
	e.waitForPool(t, token, 2)
}

func TestAutoNodeFailureCooldownAndNoNull(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.Pools[0].AutoNode = true })
	os.Remove(filepath.Join(e.nodes, "fork-all", "node.conf"))
	fake := &fakeProvisioner{nodes: e.nodes, t: t, fail: true}
	e.prov = fake
	e.start(e.srv.cfg)
	token, _ := e.register("ivan", "a")

	e.call("GET", "/api/v1/subscription", token, nil)
	// wait until the failed attempt is recorded (busy cleared, cooldown set)
	deadline := time.Now().Add(5 * time.Second)
	for {
		e.srv.w.mu.Lock()
		busy := e.srv.w.busy["fork-all"]
		_, cooled := e.srv.w.again["fork-all"]
		e.srv.w.mu.Unlock()
		if !busy && cooled {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the failed attempt did not cool down")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for i := 0; i < 3; i++ {
		e.call("GET", "/api/v1/subscription", token, nil)
	}
	if c := fake.count(); c != 1 {
		t.Fatalf("during the cooldown calls=%d, want 1", c)
	}
	e.clock.add(4 * time.Minute)
	e.call("GET", "/api/v1/subscription", token, nil)
	if c := fake.count(); c != 2 {
		t.Fatalf("after the window calls=%d, want another attempt", c)
	}
	_, doc := e.call("GET", "/api/v1/subscription", token, nil)
	if doc["profiles"] == nil {
		t.Fatalf("a broken autoNode pool must still keep the empty list, not null: %v", doc)
	}
}

func TestPlainPoolsNeverAskForTheirChannel(t *testing.T) {
	e := newEnv(t) // no autoNode anywhere
	os.Remove(filepath.Join(e.nodes, "fork-all", "node.conf"))
	fake := &fakeProvisioner{nodes: e.nodes, t: t}
	e.prov = fake
	e.start(e.srv.cfg)
	token, _ := e.register("ivan", "a")
	_, doc := e.call("GET", "/api/v1/subscription", token, nil)
	if fake.count() != 0 {
		t.Fatalf("a plain pool must not create channels: calls=%d", fake.count())
	}
	if len(doc.list("profiles")) != 1 || reply(doc.list("profiles")[0].(map[string]any)).str("id") != "de" {
		t.Fatalf("the broken plain pool is only left out: %v", doc)
	}
}

// /admin/info (opensub status) heals autoNode pools too.
func TestAdminInfoHealsAutoNodePool(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.Pools[0].AutoNode = true })
	os.Remove(filepath.Join(e.nodes, "fork-all", "node.conf"))
	fake := &fakeProvisioner{nodes: e.nodes, t: t, started: make(chan struct{})}
	e.prov = fake
	e.start(e.srv.cfg)

	code, info := e.admin("GET", "/admin/info", nil)
	if code != 200 {
		t.Fatalf("info: %d", code)
	}
	if len(info.list("pools")) != 2 {
		t.Fatalf("pools: %v", info)
	}
	select {
	case <-fake.started:
	case <-time.After(5 * time.Second):
		t.Fatal("adminInfo did not ask for the channel")
	}
}

// A pool whose channel works (already on disk) must not ask for it again.
func TestAutoNodeHealthyPoolIsNotTouched(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.Pools[0].AutoNode = true }) // fork-all intact
	fake := &fakeProvisioner{nodes: e.nodes, t: t}
	e.prov = fake
	e.start(e.srv.cfg)
	token, _ := e.register("ivan", "a")
	_, doc := e.call("GET", "/api/v1/subscription", token, nil)
	if fake.count() != 0 {
		t.Fatalf("a healthy channel must not be created: calls=%d", fake.count())
	}
	if len(doc.list("profiles")) != 2 {
		t.Fatalf("profiles: %v", doc)
	}
}

func TestPerUserConfigAndChannelName(t *testing.T) {
	c := &Config{
		PublicHost: "h", AdminToken: strings.Repeat("x", 24),
		Pools: []Pool{{ID: "main", Name: "A", PerUser: true,
			Transports: []provision.ChannelTransport{{Type: "cupsonline"}, {Type: "direct"}}}},
		Plans: []Plan{{ID: "p", Name: "P", Days: 1, Devices: 1}},
	}
	if err := c.Check(); err != nil {
		t.Fatalf("perUser pool without channel must pass: %v", err)
	}
	// Deterministic, stable, one per owner, valid for node-install.sh.
	a1 := userChannel("account-1")
	a2 := userChannel("account-1")
	if a1 != a2 {
		t.Fatalf("channel name is not deterministic: %q vs %q", a1, a2)
	}
	if !scriptChannelRe.MatchString(a1) {
		t.Fatalf("channel %q does not fit node-install.sh", a1)
	}
	if userChannel("account-1") == userChannel("account-2") {
		t.Fatal("two owners got the same channel")
	}
	// A perUser pool with a bad transport template is refused.
	bad := *c
	bad.Pools = []Pool{{ID: "main", Name: "A", PerUser: true,
		Transports: []provision.ChannelTransport{{Type: "vyandex"}}}}
	if err := bad.Check(); err == nil {
		t.Fatal("a yandex transport without url must be refused")
	}
}

func TestPerUserGetsOwnChannel(t *testing.T) {
	e := newEnv(t, func(c *Config) {
		c.Pools = []Pool{{ID: "main", Name: "Главная", PerUser: true,
			Transports: []provision.ChannelTransport{{Type: "cupsonline"}}}}
		c.Plans = []Plan{{ID: "trial", Name: "Пробный", Days: 3, Devices: 1, Trial: true}}
	})
	fake := &fakeProvisioner{nodes: e.nodes, t: t}
	e.prov = fake
	e.start(e.srv.cfg)

	t1, _ := e.register("ivan", "a")
	t2, _ := e.register("petr", "b")

	// Both ask: each triggers a creation of their own channel.
	if _, d1 := e.call("GET", "/api/v1/subscription", t1, nil); d1["profiles"] == nil {
		t.Fatalf("during per-user channel creation the answer must be \"profiles\":[], not null: %v", d1)
	}
	e.call("GET", "/api/v1/subscription", t2, nil)
	u1, u2 := userChannel(e.idOf("ivan")), userChannel(e.idOf("petr"))
	if u1 == u2 {
		t.Fatal("two owners must not share a channel")
	}
	// The creation is async (start spawns a goroutine): wait for both channels.
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, e1 := os.Stat(filepath.Join(e.nodes, u1, "node.conf"))
		_, e2 := os.Stat(filepath.Join(e.nodes, u2, "node.conf"))
		if e1 == nil && e2 == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("channels were not created: ivan %v, petr %v", e1, e2)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// ivan now sees his own profile.
	e.waitForPool(t, t1, 1)
	_, doc := e.call("GET", "/api/v1/subscription", t1, nil)
	if len(doc.list("profiles")) != 1 {
		t.Fatalf("profiles: %v", doc)
	}
	cfgL, err := share.Decode(reply(doc.list("profiles")[0].(map[string]any)).str("link"))
	if err != nil {
		t.Fatal(err)
	}
	keyBytes, err := os.ReadFile(filepath.Join(e.nodes, u1, "encryption-key"))
	if err != nil {
		t.Fatal(err)
	}
	if cfgL.Secret != strings.TrimSpace(string(keyBytes)) {
		t.Fatalf("ivan's profile must be his own channel: link secret %q, channel key %q", cfgL.Secret, strings.TrimSpace(string(keyBytes)))
	}
}

func TestPerUserHealthyChannelIsNotRecreated(t *testing.T) {
	e := newEnv(t, func(c *Config) {
		c.Pools = []Pool{{ID: "main", Name: "Главная", PerUser: true,
			Transports: []provision.ChannelTransport{{Type: "cupsonline"}}}}
		c.Plans = []Plan{{ID: "trial", Name: "Пробный", Days: 3, Devices: 1, Trial: true}}
	})
	fake := &fakeProvisioner{nodes: e.nodes, t: t}
	e.prov = fake
	e.start(e.srv.cfg)
	token, _ := e.register("ivan", "a")
	e.waitForPool(t, token, 1)
	before := fake.count()
	e.call("GET", "/api/v1/subscription", token, nil)
	if fake.count() != before {
		t.Fatalf("a ready channel was recreated: %d -> %d", before, fake.count())
	}
}

// A key without an account gets its own channel too.
func TestPerUserKeyGetsOwnChannel(t *testing.T) {
	e := newEnv(t, func(c *Config) {
		c.Pools = []Pool{{ID: "main", Name: "Главная", PerUser: true,
			Transports: []provision.ChannelTransport{{Type: "cupsonline"}}}}
		c.Plans = []Plan{{ID: "trial", Name: "Пробный", Days: 3, Devices: 1, Trial: true}}
	})
	fake := &fakeProvisioner{nodes: e.nodes, t: t}
	e.prov = fake
	e.start(e.srv.cfg)
	_, k := e.admin("POST", "/admin/keys", map[string]any{"name": "Подарок"})
	kid := k.str("id")
	token := k.str("token")
	e.call("GET", "/sub/"+token, "", nil)
	// The key's channel appears (id is the key's; wait by polling profiles).
	var doc reply
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, doc = e.call("GET", "/sub/"+token, "", nil)
		if len(doc.list("profiles")) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the key never got a channel: %v", doc)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cfgL, err := share.Decode(reply(doc.list("profiles")[0].(map[string]any)).str("link"))
	if err != nil {
		t.Fatal(err)
	}
	keyBytes, err := os.ReadFile(filepath.Join(e.nodes, userChannel(kid), "encryption-key"))
	if err != nil {
		t.Fatal(err)
	}
	if cfgL.Secret != strings.TrimSpace(string(keyBytes)) {
		t.Fatalf("the key's profile must be its own channel: link secret %q, channel key %q", cfgL.Secret, strings.TrimSpace(string(keyBytes)))
	}
}
