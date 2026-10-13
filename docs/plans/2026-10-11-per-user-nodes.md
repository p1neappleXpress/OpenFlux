# Персональная нода на подписчика — план реализации

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Каждый подписчик получает личную выходную ноду (канал `u<hex>`) с собственными cups.online комнатами и портом; слот `DocFactory` готов принять служебные аккаунты для документов vyandex/mailru/boards (этап 2).

**Architecture:** Пул с `perUser` не имеет фиксированного `channel` — имя выводится детерминированно из владельца (`u` + 18 hex sha256(owner)). Создание лениво на первом запросе подписки, через уже существующий single-flight `nodeWatch`, но ключ — имя канала. Документы-транспорты добываются через интерфейс `DocFactory` (по умолчанию аккаунтов нет — такие транспорты опускаются).

**Tech Stack:** Go 1.x, `subscription/server` (opensub), `deploy/node-install.sh`, `transport/cupsonline`.

**Дизайн:** `docs/plans/2026-10-11-per-user-nodes-design.md`.

**Важно:** движок `node-install.sh` не меняется. `Ensure` шлёт конфиг как `plan` (stdin) и `apply` (файлом) — как уже сделано в `node.go`.

---

## Task 1: `perUser` в конфиге + детерминированное имя канала

**Files:**
- Modify: `subscription/server/config.go`
- Test: `subscription/server/heal_test.go`

**Step 1: Write the failing test**

Добавить в конец `subscription/server/heal_test.go`:

```go
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
```

**Step 2: Run test to verify it fails**

Run: `go test ./subscription/server -run TestPerUserConfigAndChannelName -v`
Expected: FAIL — `undefined: userChannel` (и/или пул без channel не проходит Check).

**Step 3: Write minimal implementation**

В `subscription/server/config.go`, в `import` добавить `"crypto/sha256"` и `"encoding/hex"`:

```go
import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/p1neappleXpress/OpenFlux/provision"
)
```

В структуру `Pool` добавить поле после `AutoNode`:

```go
	// PerUser makes every owner (account or key) get their own channel, built
	// from this pool's transports template. The channel name is derived from
	// the owner, so there is no fixed Channel.
	PerUser bool `json:"perUser,omitempty"`
```

В `Check()`, в цикле по пулам, заменить блок проверки пула на:

```go
	for i := range c.Pools {
		p := &c.Pools[i]
		if !idRe.MatchString(p.ID) || p.Name == "" {
			return fmt.Errorf("config: pool %q: id (a-z0-9-_) and name are required", p.ID)
		}
		if pools[p.ID] {
			return fmt.Errorf("config: pool %q twice", p.ID)
		}
		pools[p.ID] = true
		if p.Host == "" {
			p.Host = c.PublicHost
		}
		if p.PerUser {
			if _, err := provision.CheckMode(p.Mode); err != nil {
				return fmt.Errorf("config: pool %q: %w", p.ID, err)
			}
			if err := checkAutoTransports(p.Transports); err != nil {
				return fmt.Errorf("config: pool %q: %w", p.ID, err)
			}
			continue
		}
		if !idRe.MatchString(p.Channel) {
			return fmt.Errorf("config: pool %q: id, name and channel (a-z0-9-_) are required", p.ID)
		}
		if p.AutoNode {
			if !scriptChannelRe.MatchString(p.Channel) {
				return fmt.Errorf("config: pool %q: autoNode wants a channel name like node-install.sh: a-z0-9 и дефис, до 31 символа", p.ID)
			}
			if _, err := provision.CheckMode(p.Mode); err != nil {
				return fmt.Errorf("config: pool %q: %w", p.ID, err)
			}
			if err := checkAutoTransports(p.Transports); err != nil {
				return fmt.Errorf("config: pool %q: %w", p.ID, err)
			}
		} else if hasEmptyCups(p.Transports) {
			return fmt.Errorf("config: pool %q: cupsonline без url требует autoNode", p.ID)
		}
	}
```

В конце `config.go` добавить:

```go
// userChannel is the personal channel name for an owner (account or key id):
// "u" and 18 hex of sha256(owner), which node-install.sh accepts.
func userChannel(owner string) string {
	sum := sha256.Sum256([]byte(owner))
	return "u" + hex.EncodeToString(sum[:9])
}
```

**Step 4: Run test to verify it passes**

Run: `go test ./subscription/server -run TestPerUserConfigAndChannelName -v`
Expected: PASS.

**Step 5: Commit**

```bash
git add subscription/server/config.go subscription/server/heal_test.go
git commit -m "subscription/config: perUser pool and derived channel name"
```

---

## Task 2: Запрос провиджининга `nodeRequest` + имя канала в `linkCache`

Интерфейс `Ensure` перестаёт получать `Pool` и получает `nodeRequest{Pool, Channel, Owner}`. Кэш ссылок ключуется именем канала, а не id пула.

**Files:**
- Modify: `subscription/server/node.go`
- Modify: `subscription/server/links.go`
- Modify: `subscription/server/watch.go`
- Modify: `subscription/server/server.go`
- Test: `subscription/server/node_test.go`

**Step 1: Write the failing test**

В `subscription/server/node_test.go` заменить тип вызова в третьем тесте (`Ensure` принимает запрос). Полный новый тест добавить:

```go
func TestScriptProvisionerUsesRequestChannel(t *testing.T) {
	var applyConf string
	sp := &scriptProvisioner{
		cfg:  &Config{NodeScript: "/node-install.sh"},
		logf: func(string, ...any) {},
		exec: func(_ context.Context, stdin string, name string, args ...string) ([]byte, error) {
			if name != "sh" {
				t.Fatalf("name %q, want sh", name)
			}
			switch modeOf(args) {
			case "plan":
				return []byte(`{"ok":true,"port":30001}` + "\n"), nil
			case "apply":
				b, _ := os.ReadFile(args[len(args)-1])
				applyConf = string(b)
				return []byte(`{"ok":true}` + "\n"), nil
			}
			return nil, nil
		},
		rooms: func(context.Context) (string, error) { return "WyJyb29tIl0", nil },
	}
	r := nodeRequest{Pool: Pool{ID: "main", Name: "M", PerUser: true}, Channel: "u1a2b3c4d5e6f708090", Owner: "acc-1"}
	if err := sp.Ensure(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(applyConf, "channel=u1a2b3c4d5e6f708090") {
		t.Fatalf("apply must use the request's channel, got: %q", applyConf)
	}
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./subscription/server -run TestScriptProvisionerUsesRequestChannel -v`
Expected: FAIL — `undefined: nodeRequest` / `cannot use r as Pool`.

**Step 3: Write minimal implementation**

В `subscription/server/node.go` заменить интерфейс и `Ensure`:

```go
// nodeRequest is one channel to create: the pool's template, the concrete
// channel name and the owner (account or key id) documents are made for.
type nodeRequest struct {
	Pool    Pool
	Channel string
	Owner   string
}

// nodeProvisioner creates a channel on this host. The heavy lifting is
// node-install.sh's plan/apply (as root); a cupsonline transport with an empty
// url means "make the rooms yourself". See docs/plans.
type nodeProvisioner interface {
	Ensure(ctx context.Context, r nodeRequest) error
}
```

В `Ensure` заменить сигнатуру и обращение к каналу:

```go
func (p *scriptProvisioner) Ensure(ctx context.Context, r nodeRequest) error {
	ts, err := p.channelTransports(ctx, r.Pool.Transports, r.Owner)
	if err != nil {
		return err
	}
	mode, _ := provision.CheckMode(r.Pool.Mode) // checked in Config.Check already
	key, err := provision.NewKey()
	if err != nil {
		return err
	}
	cfg := "channel=" + r.Channel + "\n" + transportLines(ts)
	if mode == provision.ModeL3 {
		cfg += "mode=l3\n"
	}
	cfg += "autoupdate=no\n"

	res, err := p.runScript(ctx, "plan", cfg)
	if err != nil {
		return err
	}
	if res.Port <= 0 || res.Port > 65535 {
		return fmt.Errorf("план ноды не дал порт (port=%d)", res.Port)
	}
	cfg += "key=" + key + "\n"
	cfg += "port=" + strconv.Itoa(res.Port) + "\n"
	if _, err := p.runScript(ctx, "apply", cfg); err != nil {
		return err
	}
	p.logf("pool %s: channel %s created on this host, port %d", r.Pool.ID, r.Channel, res.Port)
	return nil
}
```

(Пока сигнатуру `channelTransports` расширяем в Task 3; здесь достаточно передать `r.Owner`. Если собираешь строго по шагам — сделай Task 3 сразу после, тест Task 2 всё равно зелёный.)

В `subscription/server/links.go`:

```go
func (c *linkCache) link(p Pool, channel string) (string, error) {
	base := filepath.Join(c.dir, channel)
	ci, err := os.Stat(filepath.Join(base, "node.conf"))
	if err != nil {
		return "", err
	}
	ki, err := os.Stat(filepath.Join(base, "encryption-key"))
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[channel]; ok && e.conf.Equal(ci.ModTime()) && e.key.Equal(ki.ModTime()) {
		return e.link, nil
	}
	info, err := readChannel(c.dir, channel)
	if err != nil {
		return "", err
	}
	link, err := provision.ShareLinkContext(p.Name, info.key, p.Host, info.port, info.transports, info.context)
	if err != nil {
		return "", err
	}
	c.entries[channel] = cached{conf: ci.ModTime(), key: ki.ModTime(), link: link}
	return link, nil
}
```

В `subscription/server/watch.go` заменить `start`, оставив ключи-каналы:

```go
func (s *Server) start(r nodeRequest) {
	s.w.mu.Lock()
	if s.w.busy[r.Channel] {
		s.w.mu.Unlock()
		return
	}
	if t, ok := s.w.again[r.Channel]; ok && s.now().Before(t) {
		s.w.mu.Unlock()
		return
	}
	if _, err := s.links.link(r.Pool, r.Channel); err == nil {
		s.w.mu.Unlock()
		return
	}
	s.w.busy[r.Channel] = true
	s.w.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		err := s.prov.Ensure(ctx, r)
		s.w.mu.Lock()
		defer s.w.mu.Unlock()
		s.w.busy[r.Channel] = false
		if err != nil {
			s.logf("pool %s: channel %s: autoNode: %v", r.Pool.ID, r.Channel, err)
			s.w.again[r.Channel] = s.now().Add(nodeRetryAfter)
			return
		}
		delete(s.w.again, r.Channel)
	}()
}
```

В `ensureAll` (только общие autoNode-пулы; персональные создаются лениво):

```go
func (s *Server) ensureAll() {
	for _, p := range s.cfg.Pools {
		if p.AutoNode && !p.PerUser {
			s.start(nodeRequest{Pool: p, Channel: p.Channel})
		}
	}
}
```

В `subscription/server/server.go` в `profiles` временного вызова нет; но `admin.go`/`links.go` зовут `s.start(...)` — их поправит Task 4. Чтобы пакет собирался сейчас, обнови оба вызова на `s.start(nodeRequest{Pool: p, Channel: p.Channel})`.

В `server.go` `New()` дефолтный провижионер остаётся, но `channelTransports` ещё без owner — см. Task 3.

**Step 4: Run test to verify it passes**

Run: `go test ./subscription/server -run TestScriptProvisioner -v`
Expected: PASS (остальные тесты могут временно не собираться из-за вызовов `link`/`start` — их поправляет Task 4; здесь допустимо `go vet` лишь по новым функциям).

**Step 5: Commit**

```bash
git add subscription/server/node.go subscription/server/links.go subscription/server/watch.go subscription/server/server.go subscription/server/node_test.go
git commit -m "subscription: provision by channel name (nodeRequest)"
```

---

## Task 3: Слот `DocFactory` для документов

Документы vyandex/mailru/boards добываются фабрикой. По умолчанию аккаунтов нет — транспорты этого типа опускаются (в лог). cups.online комнаты и direct не трогаются.

**Files:**
- Create: `subscription/server/docs.go`
- Modify: `subscription/server/node.go`
- Modify: `subscription/server/config.go`
- Test: `subscription/server/docs_test.go`

**Step 1: Write the failing test**

Create `subscription/server/docs_test.go`:

```go
package server

import (
	"context"
	"strings"
	"testing"

	"github.com/p1neappleXpress/OpenFlux/provision"
)

func TestDocFactorySlotDropsUnconfiguredCarriers(t *testing.T) {
	sp := &scriptProvisioner{
		cfg:  &Config{NodeScript: "/n.sh"},
		logf: func(string, ...any) {},
		docs: accountDocFactory{}, // no accounts configured
		rooms: func(context.Context) (string, error) { return "WyJyb29tIl0", nil },
	}
	ts, err := sp.channelTransports(context.Background(), []provision.ChannelTransport{
		{Type: "cupsonline"}, {Type: "vyandex"}, {Type: "mailru"}, {Type: "boards"},
	}, "owner-1")
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, x := range ts {
		types = append(types, x.Type)
	}
	if strings.Join(types, ",") != "cupsonline" {
		t.Fatalf("only cups survives without accounts, got %v", types)
	}
}

func TestDocFactoryMakesDocumentWhenConfigured(t *testing.T) {
	sp := &scriptProvisioner{
		cfg:  &Config{NodeScript: "/n.sh"},
		logf: func(string, ...any) {},
		docs: fakeDocFactory{url: "https://docs.yandex.ru/edit/d/abcdefghijklmnopqrstuv"},
		rooms: func(context.Context) (string, error) { return "WyJyb29tIl0", nil },
	}
	ts, err := sp.channelTransports(context.Background(), []provision.ChannelTransport{
		{Type: "cupsonline"}, {Type: "vyandex"},
	}, "owner-1")
	if err != nil {
		t.Fatal(err)
	}
	var got string
	for _, x := range ts {
		if x.Type == "vyandex" {
			got = x.URL
		}
	}
	if got != "https://docs.yandex.ru/edit/d/abcdefghijklmnopqrstuv" {
		t.Fatalf("the factory's document did not reach the transports: %v", ts)
	}
}

type fakeDocFactory struct{ url string }

func (f fakeDocFactory) Make(context.Context, string, string) (string, string, error) {
	return f.url, "cookies", nil
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./subscription/server -run TestDocFactory -v`
Expected: FAIL — `undefined: accountDocFactory`, `sp.docs`, `channelTransports` arity.

**Step 3: Write minimal implementation**

Create `subscription/server/docs.go`:

```go
package server

import (
	"context"
	"errors"
)

// DocFactory makes a document for one owner at one carrier (vyandex, mailru,
// boards) and returns its url plus the cookies a node reads it with. It is the
// slot for the office accounts: without them there are no documents.
type DocFactory interface {
	Make(ctx context.Context, carrier, owner string) (url, cookies string, err error)
}

// errNoDocAccount is what a factory without an office account says.
var errNoDocAccount = errors.New("нет служебного аккаунта")

// accountDocFactory uses the office accounts from the config. Until those
// accounts exist it always reports errNoDocAccount and callers drop the
// carrier, so a channel is still created with what does work.
type accountDocFactory struct{}

func (accountDocFactory) Make(context.Context, string, string) (string, string, error) {
	return "", "", errNoDocAccount
}

// docCarrier reports whether a transport type needs a document from a factory.
func docCarrier(typ string) bool {
	switch typ {
	case "vyandex", "yandex", "mailru", "boards":
		return true
	}
	return false
}
```

В `subscription/server/node.go` добавить поле `docs` и обновить `channelTransports`:

```go
type scriptProvisioner struct {
	cfg  *Config
	logf func(format string, args ...any)
	exec func(ctx context.Context, stdin string, name string, args ...string) ([]byte, error)
	rooms func(ctx context.Context) (string, error)
	// docs supplies carrier documents (vyandex/mailru/boards); nil or an
	// unconfigured factory means those carriers are skipped.
	docs DocFactory
}
```

```go
// channelTransports fills a cupsonline transport with empty url with fresh
// cups.online rooms, asks the DocFactory for document carriers, and drops a
// document carrier no factory can serve. A pool with no transports at all gets
// cups.online rooms, so a channel always has a document for readChannel.
func (p *scriptProvisioner) channelTransports(ctx context.Context, ts []provision.ChannelTransport, owner string) ([]provision.ChannelTransport, error) {
	out := make([]provision.ChannelTransport, 0, len(ts)+1)
	for _, t := range ts {
		t.Type = strings.TrimSpace(t.Type)
		t.URL = strings.TrimSpace(t.URL)
		switch {
		case t.Type == "cupsonline" && t.URL == "":
			rooms, err := p.rooms(ctx)
			if err != nil {
				return nil, fmt.Errorf("cups.online: %w", err)
			}
			t.URL = rooms
		case docCarrier(t.Type) && t.URL == "":
			if p.docs == nil {
				continue
			}
			url, _, err := p.docs.Make(ctx, t.Type, owner)
			if err != nil {
				p.logf("pool owner %s: %s: %v", owner, t.Type, err)
				continue
			}
			t.URL = url
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		rooms, err := p.rooms(ctx)
		if err != nil {
			return nil, fmt.Errorf("cups.online: %w", err)
		}
		out = append(out, provision.ChannelTransport{Type: "cupsonline", URL: rooms})
	}
	return provision.CheckTransports(out)
}
```

В `subscription/server/server.go` `New()` добавить фабрику в дефолтный провижионер:

```go
		s.prov = &scriptProvisioner{cfg: cfg, logf: s.logf, exec: runCmd, rooms: cupsonline.CreateRoomList, docs: accountDocFactory{}}
```

**Step 4: Run test to verify it passes**

Run: `go test ./subscription/server -run TestDocFactory -v`
Expected: PASS.

**Step 5: Commit**

```bash
git add subscription/server/docs.go subscription/server/docs_test.go subscription/server/node.go subscription/server/server.go
git commit -m "subscription: DocFactory slot for carrier documents"
```

---

## Task 4: Провести владельца через документ, профили и админку

`profiles` и `document` получают владельца (id аккаунта или ключа). Для `perUser`-пула канал считается из владельца; отсутствующий — триггерит создание, в документе пока `[]`.

**Files:**
- Modify: `subscription/server/links.go`
- Modify: `subscription/server/server.go`
- Modify: `subscription/server/accounts.go`
- Modify: `subscription/server/admin.go`
- Test: `subscription/server/heal_test.go`

**Step 1: Write the failing tests**

Добавить в `subscription/server/heal_test.go`:

```go
func TestPerUserGetsOwnChannel(t *testing.T) {
	e := newEnv(t, func(c *Config) {
		c.Pools[0] = Pool{ID: "main", Name: "Главная", PerUser: true,
			Transports: []provision.ChannelTransport{{Type: "cupsonline"}}}
	})
	fake := &fakeProvisioner{nodes: e.nodes, t: t}
	e.prov = fake
	e.start(e.srv.cfg)

	t1, _ := e.register("ivan", "a")
	t2, _ := e.register("petr", "b")

	// Both ask: each triggers a creation of their own channel.
	e.call("GET", "/api/v1/subscription", t1, nil)
	e.call("GET", "/api/v1/subscription", t2, nil)
	u1, u2 := userChannel(e.idOf("ivan")), userChannel(e.idOf("petr"))
	if u1 == u2 {
		t.Fatal("two owners must not share a channel")
	}
	if _, err := os.Stat(filepath.Join(e.nodes, u1, "node.conf")); err != nil {
		t.Fatalf("ivan's channel was not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.nodes, u2, "node.conf")); err != nil {
		t.Fatalf("petr's channel was not created: %v", err)
	}
	// ivan now sees his own profile.
	e.waitForPool(t, t1, 1)
	_, doc := e.call("GET", "/api/v1/subscription", t1, nil)
	if len(doc.list("profiles")) != 1 {
		t.Fatalf("profiles: %v", doc)
	}
}

func TestPerUserHealthyChannelIsNotRecreated(t *testing.T) {
	e := newEnv(t, func(c *Config) {
		c.Pools[0] = Pool{ID: "main", Name: "Главная", PerUser: true,
			Transports: []provision.ChannelTransport{{Type: "cupsonline"}}}
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
		c.Pools[0] = Pool{ID: "main", Name: "Главная", PerUser: true,
			Transports: []provision.ChannelTransport{{Type: "cupsonline"}}}
	})
	fake := &fakeProvisioner{nodes: e.nodes, t: t}
	e.prov = fake
	e.start(e.srv.cfg)
	_, k := e.admin("POST", "/admin/keys", map[string]any{"name": "Подарок"})
	token := k.str("token")
	e.call("GET", "/sub/"+token, "", nil)
	// The key's channel appears (id is the key's; wait by polling profiles).
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, doc := e.call("GET", "/sub/"+token, "", nil)
		if len(doc.list("profiles")) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the key never got a channel: %v", doc)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
```

И вспомогательный метод в `server_test.go` (рядом с `register`):

```go
func (e *env) idOf(login string) string {
	e.t.Helper()
	_, users := e.admin("GET", "/admin/users", nil)
	for _, u := range users {
		ur := reply(u.(map[string]any))
		if ur.str("login") == login {
			return ur.str("id")
		}
	}
	e.t.Fatalf("no user %q", login)
	return ""
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./subscription/server -run TestPerUser -v`
Expected: FAIL — `Too many arguments`/`not enough arguments` в `profiles`/`document`, и `perUser` не создаёт каналы.

**Step 3: Write minimal implementation**

В `subscription/server/links.go` заменить `profiles`:

```go
// profiles are the locations an owner (account or key) may use. A perUser pool
// gives each owner their own channel; a pool whose files cannot be read is left
// out: the others still work.
func (s *Server) profiles(poolIDs []string, owner string) []subscription.Profile {
	want := map[string]bool{}
	for _, id := range poolIDs {
		want[id] = true
	}
	out := []subscription.Profile{}
	for _, p := range s.cfg.Pools {
		if len(want) > 0 && !want[p.ID] {
			continue
		}
		channel := p.Channel
		if p.PerUser {
			if owner == "" {
				continue
			}
			channel = userChannel(owner)
		}
		link, err := s.links.link(p, channel)
		if err != nil {
			s.logf("pool %s: %v", p.ID, err)
			if p.AutoNode || p.PerUser {
				s.start(nodeRequest{Pool: p, Channel: channel, Owner: owner})
			}
			continue
		}
		out = append(out, subscription.Profile{ID: p.ID, Name: p.Name, Link: link})
	}
	return out
}
```

В `subscription/server/server.go` `document` получает owner:

```go
func (s *Server) document(status string, expires int64, pools []string, owner string) subscription.Document {
	d := subscription.Document{
		Version:     1,
		Title:       s.cfg.Title,
		Status:      status,
		Expires:     expires,
		UpdateHours: s.cfg.UpdateHours,
		Support:     s.cfg.Support,
		Announce:    s.cfg.Announce,
		Profiles:    []subscription.Profile{},
	}
	if status == subscription.StatusActive {
		d.Profiles = s.profiles(pools, owner)
	}
	return d
}
```

В `subscription/server/accounts.go`:

```go
func (s *Server) handleSubscription(w http.ResponseWriter, _ *http.Request, u *User, _ *Device) {
	writeJSON(w, http.StatusOK, s.document(s.userStatus(u), u.Expires, s.userPools(u), u.ID))
}
```

В `subscription/server/server.go` в `handleKey` поправить вызов `s.document` (стандартная подстановка owner = id ключа). Найти строку `writeJSON(w, http.StatusOK, s.document(...))` внутри `handleKey` и заменить owner-аргументом:

```go
		writeJSON(w, http.StatusOK, s.document(status, expires, pools, k.ID))
```

(проверь точную строку рядом с `s.document` в `handleKey`, строки ~270–290).

В `subscription/server/admin.go` `adminInfo`: для `perUser`-пула нет фиксированного канала, показываем только имя:

```go
	for _, p := range s.cfg.Pools {
		pl := pool{ID: p.ID, Name: p.Name, Channel: p.Channel, OK: true}
		if p.PerUser {
			pl.Channel = "(per-user)"
			pools = append(pools, pl)
			continue
		}
		if _, err := s.links.link(p, p.Channel); err != nil {
			pl.OK, pl.Error = false, err.Error()
			if p.AutoNode {
				s.start(nodeRequest{Pool: p, Channel: p.Channel})
			}
		}
		pools = append(pools, pl)
	}
```

**Step 4: Run test to verify it passes**

Run: `go test ./subscription/server -run TestPerUser -v`
Expected: PASS.

**Step 5: Commit**

```bash
git add subscription/server/links.go subscription/server/server.go subscription/server/accounts.go subscription/server/admin.go subscription/server/heal_test.go subscription/server/server_test.go
git commit -m "subscription: per-user channels in the subscription document"
```

---

## Task 5: Прогон всего пакета, vet, регресс

**Files:** (правки форматов если нужно)

**Step 1: Fix compile of the fake provisioner signature**

`fakeProvisioner.Ensure` и тестовые фейки должны принимать `nodeRequest`. Обновить в `subscription/server/heal_test.go`:

```go
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
```

Обновить остальные тесты, которые звали `Ensure`/`start` с `Pool` (в `heal_test.go`), на `nodeRequest`.

**Step 2: Run the whole package**

Run: `go test ./subscription/server`
Expected: PASS. Fix any leftover `link(`/`start(` arities.

**Step 3: Vet and full suite**

Run: `go vet ./subscription/server`
Run: `go test ./...`
Expected: PASS (тест `tunnel/TestL4UDPDatagramRoundTrip` — известный флейки gVisor, не связан).

**Step 4: Commit**

```bash
git add subscription/server
git commit -m "subscription: per-user nodes tests green"
```

---

## Task 6: Выпуск и миграция сервера (141.11.209.233)

**Step 1: Build the linux binary**

Run (в `OpenFlux`):
```powershell
$env:GOOS="linux"; $env:GOARCH="amd64"; $env:CGO_ENABLED="0"; go build -o opensub-linux ./cmd/opensub
```

**Step 2: Back up and edit config**

На сервере: `cp -a /etc/openflux-sub/config.json /etc/openflux-sub/config.json.bak-peruser`, затем в пуле `main` заменить на:
```json
{ "id": "main", "name": "Основной", "perUser": true, "mode": "l4",
  "transports": [ { "type": "cupsonline" }, { "type": "direct" } ] }
```
(поле `channel` убрать). Проверить: `python3 -m json.tool`.

**Step 3: Deploy and restart**

```bash
scp -o BatchMode=yes opensub-linux root@141.11.209.233:/opt/openflux-sub/opensub.new
ssh root@141.11.209.233 'chmod 0755 /opt/openflux-sub/opensub.new && cp -a /opt/openflux-sub/opensub /opt/openflux-sub/opensub.old-peruser && mv -f /opt/openflux-sub/opensub.new /opt/openflux-sub/opensub && systemctl restart openflux-sub'
```
**Обязательно `chmod 0755`** после scp — иначе 203/EXEC.

**Step 4: Verify**

```bash
ssh root@141.11.209.233 '/opt/openflux-sub/opensub status'
```
Expected: `pool main channel (per-user) ok`.

Регистрация тестового юзера и подписка:
```bash
L=verify-peruser-$RANDOM; REG=$(curl -sk -X POST https://127.0.0.1:8443/api/v1/register -H 'Content-Type: application/json' -d "{\"login\":\"$L\",\"password\":\"Passw0rd!42\",\"deviceName\":\"v\"}"); TOK=$(echo "$REG" | python3 -c 'import sys,json; print(json.load(sys.stdin)["deviceToken"])'); curl -sk -H "Authorization: Bearer $TOK" https://127.0.0.1:8443/api/v1/subscription | python3 -m json.tool
```
Expected: `profiles` содержит личный профиль; после первого запроса появляется `openflux-node@u<hex>`:
```bash
ssh root@141.11.209.233 'ls /etc/openflux-node/ ; systemctl list-units --no-pager --plain "openflux-node@*"'
```

**Step 5: Update docs and commit**

Обновить `docs/subscriptions.ru.md`: пул `perUser`, личный канал `u<hex>`, ленивое создание, слот `docAccounts` (этап 2). Закоммитить.

**Step 6: Leave the old shared channel**

`fork-all` больше не используется новыми юзерами; снести вручную при желании: `node-install.sh remove fork-all` (не автоматизируем).
