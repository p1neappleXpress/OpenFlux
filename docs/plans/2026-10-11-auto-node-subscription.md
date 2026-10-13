# Auto-создание выходной ноды пула подписки — план реализации

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** opensub сам создаёт exit-ноду на этом хосте (ключ, порт, комнаты cups.online, `node-install.sh apply`), когда файлы канала пула с `autoNode` пропали, и подписчик снова получает локацию на следующем обновлении.

**Architecture:** Пул в `config.json` получает `autoNode` + опциональные `transports` (cupsonline с пустым url = «создать комнаты самому»). В `subscription/server` добавляются: интерфейс `nodeProvisioner` (боевая реализация гоняет локальный `/opt/openflux-node/node-install.sh plan|apply` через temp-конфиг, как CLI `rotate`), и `nodeWatch` — single-flight по имени канала + cooldown при неудаче, который дёргается из `profiles()`, `/admin/info` и фоновым тикером раз в 3 минуты. Уже исправленный `profiles()` продолжает отдавать `[]`, пока канал создаётся.

**Tech Stack:** Go (opensub, `subscription/server`), `provision.NewKey`, `provision.CheckTransports`, `transport/cupsonline.CreateRoomList`, локальный `node-install.sh` v1.4.1. Тесты — только unit (фейковый провизионер; скриптовый провизионер — с инжектнутым `exec`/`rooms`). Обезвреживание — код не трогает сеть/openflux ядро.

---

## Условия объёма

- Единственный реальный сетевой вызов — `cupsonline.CreateRoomList` (комнаты чужие, без аккаунта). Всё остальное — локальный процесс `sh script plan|apply` и файлы канала.
- Канал создаётся СИНХРОННО в фоновой горутине `nodeWatch`; HTTP никогда не ждёт провижининг.
- Канал-имя авто-пула дополнительно к `idRe` должно подходить `node-install.sh`: `^[a-z0-9][a-z0-9-]{0,30}$` (без `_`).
- Сервер юнита `openflux-sub` меняется на root + `ReadWritePaths` — это шаг деплоя (Task 6), не код.
- Коммиты в шагах ниже — по желанию пользователя: сам план их содержит (серия мелких коммитов), но что именно коммитить и когда — подтверждаем с владельцем перед `git commit`.

---

### Task 1: Конфиг — `autoNode`, `transports`, `mode`, `nodeScript`

**Files:**
- Modify: `subscription/server/config.go`
- Test: `subscription/server/config.go` (таблица `TestConfigChecks` остаётся), новый `subscription/server/heal_test.go`

**Step 1.1: Добавить импорт provision в config.go**

```go
import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"

	"github.com/p1neappleXpress/OpenFlux/provision"
)
```

**Step 1.2: Поля Config и Pool** (config.go:24-55)

После `NodeConfDir` в `Config`:
```go
	// NodeConfDir is where node-install.sh keeps its channels.
	NodeConfDir string `json:"nodeConfDir,omitempty"`
	// NodeScript is node-install.sh on this machine (local, root), which the
	// autoNode pools run to create their channel.
	NodeScript string `json:"nodeScript,omitempty"`
```

`Pool` целиком:
```go
// Pool is one location: a channel of a node installed by node-install.sh.
type Pool struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Channel string `json:"channel"`
	// Host is the node's address in the link's direct carrier (default PublicHost).
	Host string `json:"host,omitempty"`
	// AutoNode makes the service itself create the channel on this host when
	// its files are missing (opensub must run as root; see docs).
	AutoNode bool `json:"autoNode,omitempty"`
	// Transports are the channel's carriers besides direct (see provision.ChannelTransport).
	// A cupsonline entry with an empty url means "create the rooms yourself" (AutoNode);
	// empty Transports + AutoNode likewise gets cups.online rooms only.
	Transports []provision.ChannelTransport `json:"transports,omitempty"`
	// Mode is the node's forwarding mode: "l4" (default) or "l3".
	Mode string `json:"mode,omitempty"`
}
```

**Step 1.3: Константа валидного имени канала** (config.go:77)

```go
var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// scriptChannelRe is node-install.sh's valid_channel: autoNode runs the script
// with the pool's channel name.
var scriptChannelRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)
```

**Step 1.4: `Check()` — дефолт и проверки** (config.go:93-157)

После `if c.NodeConfDir == "" { ... }`:
```go
	if c.NodeScript == "" {
		c.NodeScript = "/opt/openflux-node/node-install.sh"
	}
```

В цикле пулов (`for i := range c.Pools`) после `if p.Host == "" { p.Host = c.PublicHost }`:
```go
		if p.AutoNode {
			if !scriptChannelRe.MatchString(p.Channel) {
				return fmt.Errorf("config: pool %q: autoNode wants a channel name like node-install.sh: a-z0-9 и дефис, до 31 символа", p.ID)
			}
			if _, err := provision.CheckMode(p.Mode); err != nil {
				return fmt.Errorf("config: pool %q: %v", p.ID, err)
			}
			if err := checkAutoTransports(p.Transports); err != nil {
				return fmt.Errorf("config: pool %q: %v", p.ID, err)
			}
		} else if hasEmptyCups(p.Transports) {
			return fmt.Errorf("config: pool %q: cupsonline без url требует autoNode", p.ID)
		}
```

**Step 1.5: Хелперы в конец config.go** (после `trialPlan`)

```go
// hasEmptyCups reports a cupsonline transport that the service would create
// rooms for: only possible with autoNode.
func hasEmptyCups(ts []provision.ChannelTransport) bool {
	for _, t := range ts {
		if t.Type == "cupsonline" && strings.TrimSpace(t.URL) == "" {
			return true
		}
	}
	return false
}

// checkAutoTransports validates the transports of an autoNode pool: any type
// except a cupsonline-with-empty-url is checked like provision does; two of a
// type are refused.
func checkAutoTransports(ts []provision.ChannelTransport) error {
	seen := map[string]bool{}
	for _, t := range ts {
		if t.Type == "cupsonline" && strings.TrimSpace(t.URL) == "" {
			t.Type = strings.TrimSpace(t.Type)
		} else if _, err := provision.CheckTransports([]provision.ChannelTransport{t}); err != nil {
			return err
		}
		if seen[t.Type] {
			return fmt.Errorf("транспорт %s выбран дважды", t.Type)
		}
		seen[t.Type] = true
	}
	return nil
}
```
Заметка: `strings` уже импортирован в config.go? — нет, добавить в импорты.

**Step 1.6: Тесты конфига** — в `subscription/server/heal_test.go` (новый файл), в конце:

```go
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
	var empty = []provision.ChannelTransport{{Type: "cupsonline", URL: ""}}
	for name, mutate := range map[string]func(*Config){
		"underscore channel": func(c *Config) { c.Pools[0].Channel = "a_b" },
		"bad transport":      func(c *Config) { c.Pools[0].Transports = []provision.ChannelTransport{{Type: "vyandex"}} },
		"duplicate transport": func(c *Config) {
			c.Pools[0].Transports = append(c.Pools[0].Transports, empty[0])
			c.Pools[0].Transports = append(c.Pools[0].Transports, empty[0])
		},
		"yandex empty url": func(c *Config) { c.Pools[0].Transports = []provision.ChannelTransport{{Type: "yandex"}} },
		"cups without autoNode": func(c *Config) { c.Pools[0].AutoNode = false },
		"a-bad mode":        func(c *Config) { c.Pools[0].Mode = "l9" },
	} {
		c := good()
		mutate(c)
		if err := c.Check(); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if got := good(); got.Check() != nil {
		t.Fatal(got.Check())
	}
	if got := (&Config{PublicHost: "h", AdminToken: strings.Repeat("x", 24),
		Pools: []Pool{{ID: "a", Name: "A", Channel: "c"}}, Plans: []Plan{{ID: "p", Name: "P", Days: 1, Devices: 1}}}).Check(); got != nil {
		t.Fatal(got)
	}
}
```
(случаи `"cups without autoNode"` и `"yandex empty url"` — понадобятся импорты `strings` и `provision` в тесте.)

**Step 1.7: Запустить тесты**

```
go test ./subscription/server
```
Ожидание: `ok github.com/p1neappleXpress/OpenFlux/subscription/server` (новые и старые тесты проходят; `TestConfigChecks` не задет).

**Step 1.8: Commit**

```bash
git add subscription/server/config.go subscription/server/heal_test.go
git commit -m "subscription: pools can create their node themselves (autoNode)"
```

---

### Task 2: `nodeProvisioner` и скриптовый провизионер

**Files:**
- Create: `subscription/server/node.go`
- Test: `subscription/server/node_test.go`

Боевая реализация не трогает IPTables/ядро l3, ключи не логирует, простые права root.

**Step 2.1: Написать `subscription/server/node.go` целиком**

```go
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/p1neappleXpress/OpenFlux/provision"
	"github.com/p1neappleXpress/OpenFlux/transport/cupsonline"
)

// nodeProvisioner creates a pool's exit node channel on this host. The heavy
// lifting is node-install.sh's plan/apply (as root); a cupsonline transport
// with an empty url means "make the rooms yourself". See docs/plans.
type nodeProvisioner interface {
	Ensure(ctx context.Context, p Pool) error
}

// scriptProvisioner runs node-install.sh on this machine, the way the owner
// does from the CLI, so a missing channel is made without their hands.
type scriptProvisioner struct {
	cfg   *Config
	logf  func(format string, args ...any)
	// exec runs the script; runCmd runs it for real. Tests replace it and
	// return node-install.sh answers without a shell.
	exec func(ctx context.Context, name string, args ...string) ([]byte, error)
	// rooms makes cups.online rooms; cupsonline.CreateRoomList for real.
	rooms func(ctx context.Context) (string, error)
}

// scriptResult is node-install.sh's last JSON line.
type scriptResult struct {
	OK    bool   `json:"ok"`
	Port  int    `json:"port"`
	Error string `json:"error"`
}

func runCmd(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// channelTransports fills a cupsonline transport with empty url with fresh
// cups.online rooms, and makes rooms for a pool with no transports at all, so
// a new channel always has a document transport for readChannel.
func (p *scriptProvisioner) channelTransports(ctx context.Context, ts []provision.ChannelTransport) ([]provision.ChannelTransport, error) {
	out := make([]provision.ChannelTransport, 0, len(ts)+1)
	for _, t := range ts {
		t.URL = strings.TrimSpace(t.URL)
		if t.Type == "cupsonline" && t.URL == "" {
			rooms, err := p.rooms(ctx)
			if err != nil {
				return nil, fmt.Errorf("cups.online: %w", err)
			}
			t.URL = rooms
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

// Ensure creates the pool's channel on this host: node-install.sh plan picks a
// port, apply installs, configures and starts the openflux-node@channel unit.
func (p *scriptProvisioner) Ensure(ctx context.Context, pool Pool) error {
	ts, err := p.channelTransports(ctx, pool.Transports)
	if err != nil {
		return err
	}
	mode, _ := provision.CheckMode(pool.Mode) // checked in Config.Check already
	key, err := provision.NewKey()
	if err != nil {
		return err
	}
	cfg := "channel=" + pool.Channel + "\n" + transportLines(ts)
	if mode == provision.ModeL3 {
		cfg += "mode=l3\n"
	}
	cfg += "autoupdate=no\n"

	r, err := p.runScript(ctx, "plan", cfg)
	if err != nil {
		return err
	}
	if r.Port <= 0 || r.Port > 65535 {
		return fmt.Errorf("план ноды не дал порт (port=%d)", r.Port)
	}
	cfg += "key=" + key + "\n"
	cfg += "port=" + strconv.Itoa(r.Port) + "\n"
	if _, err := p.runScript(ctx, "apply", cfg); err != nil {
		return err
	}
	p.logf("pool %s: channel %s created on this host, port %d", pool.ID, pool.Channel, r.Port)
	return nil
}

// runScript writes the config to a 0600 temp file argv-style (node-install.sh
// reads it and deletes it) and runs `sh script <cmd> <conf>`.
func (p *scriptProvisioner) runScript(ctx context.Context, cmd, cfg string) (scriptResult, error) {
	f, err := os.CreateTemp("", "opensub-node-*.conf")
	if err != nil {
		return scriptResult{}, err
	}
	defer os.Remove(f.Name())
	if err := os.WriteFile(f.Name(), []byte(cfg), 0o600); err != nil {
		return scriptResult{}, err
	}
	out, err := p.exec(ctx, "sh", p.cfg.NodeScript, cmd, f.Name())
	if err != nil {
		return scriptResult{}, fmt.Errorf("%s: %s", cmd, firstLine(out))
	}
	r, err := lastScriptJSON(out)
	if err != nil {
		return scriptResult{}, fmt.Errorf("%s: %v", cmd, err)
	}
	if !r.OK {
		return scriptResult{}, errors.New(r.Error)
	}
	return r, nil
}

// lastScriptJSON finds node-install.sh's last JSON line, like provision.lastJSON.
func lastScriptJSON(out []byte) (scriptResult, error) {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if strings.HasPrefix(l, "{") {
			var r scriptResult
			if err := json.Unmarshal([]byte(l), &r); err != nil {
				return scriptResult{}, err
			}
			return r, nil
		}
	}
	return scriptResult{}, errors.New("node-install.sh не ответил")
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
```

Заметка: функция `transportLines` здесь — дубль из `provision` (там экспорт не сделать без затрагивания клиентов; держим локальную):
```go
func transportLines(ts []provision.ChannelTransport) string {
	var b strings.Builder
	for _, t := range ts {
		b.WriteString(t.Type + "=" + t.URL + "\n")
	}
	return b.String()
}
```

**Step 2.2: Тест `subscription/server/node_test.go`** — инжект `exec`+`rooms`, никакого bash/сети:

```go
package server

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/p1neappleXpress/OpenFlux/provision"
)

func TestScriptProvisionerCallsPlanThenApply(t *testing.T) {
	cfg := &Config{NodeScript: "/opt/openflux-node/node-install.sh"}
	var planConf, applyConf string
	var cmds []string
	sp := &scriptProvisioner{
		cfg:  cfg,
		logf: func(string, ...any) {},
		exec: func(_ context.Context, name string, args ...string) ([]byte, error) {
			if name != "sh" {
				t.Fatalf("name %q, want sh", name)
			}
			cmd := args[len(args)-2]
			conf := args[len(args)-1]
			b, err := os.ReadFile(conf)
			if err != nil {
				t.Fatal(err)
			}
			cmds = append(cmds, cmd+":"+string(b))
			switch cmd {
			case "plan":
				planConf = string(b)
				return []byte(`{"ok":true,"channel":"fork-all","port":23234,"actions":["x"]}` + "\n"), nil
			case "apply":
				applyConf = string(b)
				return []byte(`{"ok":true,"channel":"fork-all","port":23234}` + "\n"), nil
			}
			return nil, nil
		},
		rooms: func(context.Context) (string, error) { return "WyJyb29tMSIsInJvb20yIl0", nil },
	}
	p := Pool{ID: "main", Channel: "fork-all", AutoNode: true,
		Transports: []provision.ChannelTransport{{Type: "cupsonline", URL: ""}}}
	if err := sp.Ensure(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if len(cmds) != 2 {
		t.Fatalf("commands %d, want plan+apply", len(cmds))
	}
	for _, want := range []string{"channel=fork-all", "cupsonline=WyJyb29tMSIsInJvb20yIl0", "autoupdate=no"} {
		if !strings.Contains(planConf, want) {
			t.Fatalf("plan conf misses %q: %q", want, planConf)
		}
	}
	if strings.Contains(planConf, "key=") {
		t.Fatal("plan must not carry the key")
	}
	for _, want := range []string{"key=", "port=23234"} {
		if !strings.Contains(applyConf, want) {
			t.Fatalf("apply conf misses %q: %q", want, applyConf)
		}
	}
	for _, line := range strings.Split(applyConf, "\n") {
		if strings.HasPrefix(line, "key=") && len(strings.TrimPrefix(line, "key=")) != 64 {
			t.Fatalf("key length: %q", line)
		}
	}
}

func TestScriptProvisionerEmptyTransportsMakeCupsRooms(t *testing.T) {
	var applyConf string
	sp := &scriptProvisioner{
		cfg:  &Config{NodeScript: "/node-install.sh"},
		logf: func(string, ...any) {},
		exec: func(_ context.Context, name string, args ...string) ([]byte, error) {
			b, _ := os.ReadFile(args[len(args)-1])
			switch args[len(args)-2] {
			case "plan":
				return []byte(`{"ok":true,"port":30001}` + "\n"), nil
			case "apply":
				applyConf = string(b)
				return []byte(`{"ok":true}` + "\n"), nil
			}
			return nil, nil
		},
		rooms: func(context.Context) (string, error) { return "WyJyb29tIl0", nil },
	}
	if err := sp.Ensure(context.Background(), Pool{ID: "m", Channel: "fork-all", AutoNode: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(applyConf, "cupsonline=WyJyb29tIl0") {
		t.Fatalf("roomless autoNode pool must get cups rooms: %q", applyConf)
	}
}

func TestScriptProvisionerReportsScriptErrors(t *testing.T) {
	sp := &scriptProvisioner{
		cfg:  &Config{NodeScript: "/node-install.sh"},
		logf: func(string, ...any) {},
		exec: func(_ context.Context, name string, args ...string) ([]byte, error) {
			if args[len(args)-2] == "plan" {
				return []byte(`{"ok":false,"error":"имя канала занято"}` + "\n"), nil
			}
			return nil, nil
		},
		rooms: func(context.Context) (string, error) { return "WyJyb29tIl0", nil },
	}
	err := sp.Ensure(context.Background(), Pool{ID: "m", Channel: "fork-all", AutoNode: true,
		Transports: []provision.ChannelTransport{{Type: "cupsonline", URL: "WyJyb29tIl0"}}})
	if err == nil || !strings.Contains(err.Error(), "имя канала занято") {
		t.Fatalf("err %v", err)
	}
}
```
(`bytes` в импортах не нужен — убрать.)

**Step 2.3: Запустить тесты**

```
go test ./subscription/server
```
Ожидание: PASS, включая новые `TestScriptProvisioner*`.

**Step 2.4: Commit**

```bash
git add subscription/server/node.go subscription/server/node_test.go
git commit -m "subscription: scriptProvisioner creates a pool's channel via node-install.sh"
```

---

### Task 3: `nodeWatch` — single-flight, cooldown, фоновый обход

**Files:**
- Modify: `subscription/server/server.go` (Server struct, Options, New, Serve)
- Modify: `subscription/server/links.go` (`profiles()`)
- Modify: `subscription/server/admin.go` (`adminInfo`)
- Create: `subscription/server/watch.go`

**Step 3.1: `subscription/server/watch.go` целиком**

```go
package server

import (
	"context"
	"sync"
	"time"
)

// nodeCheckEvery is the interval of the background pass that heals the
// autoNode pools whose channel left.
const nodeCheckEvery = 3 * time.Minute

// nodeRetryAfter is how long a failed creation waits before the next attempt.
const nodeRetryAfter = 3 * time.Minute

// nodeWatch makes sure a missing channel of an autoNode pool gets created,
// one attempt at a time, and not more often than one failure test per window.
// It never blocks an HTTP handler: the creation goes to a goroutine.
type nodeWatch struct {
	mu    sync.Mutex
	busy  map[string]bool          // channel names being created
	again map[string]time.Time     // channel -> try again not before
}

func newNodeWatch() *nodeWatch {
	return &nodeWatch{busy: map[string]bool{}, again: map[string]time.Time{}}
}

// start asks for the pool's channel to be created. It checks the channel on
// disk again under the lock, so a parallel or finished attempt is not doubled.
func (s *Server) start(p Pool) {
	s.w.mu.Lock()
	if s.w.busy[p.Channel] {
		s.w.mu.Unlock()
		return
	}
	if t, ok := s.w.again[p.Channel]; ok && s.now().Before(t) {
		s.w.mu.Unlock()
		return
	}
	if _, err := s.links.link(p); err == nil {
		s.w.mu.Unlock()
		return
	}
	s.w.busy[p.Channel] = true
	s.w.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		err := s.prov.Ensure(ctx, p)
		s.w.mu.Lock()
		defer s.w.mu.Unlock()
		s.w.busy[p.Channel] = false
		if err != nil {
			s.logf("pool %s: channel %s: autoNode: %v", p.ID, p.Channel, err)
			s.w.again[p.Channel] = s.now().Add(nodeRetryAfter)
			return
		}
		delete(s.w.again, p.Channel)
	}()
}

// startNodeWatch heals the autoNode pools at once, then every nodeCheckEvery,
// until ctx ends. Called from Serve.
func (s *Server) startNodeWatch(ctx context.Context) {
	s.ensureAll()
	go func() {
		ticker := time.NewTicker(nodeCheckEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.ensureAll()
			}
		}
	}()
}

func (s *Server) ensureAll() {
	for _, p := range s.cfg.Pools {
		if p.AutoNode {
			s.start(p)
		}
	}
}
```

**Step 3.2: `server.go`** — поля и инициализация.

Server struct: добавить после `links`:
```go
	s.links *linkCache
	prov   nodeProvisioner
	w      *nodeWatch
```

Options: добавить
```go
	// Provisioner makes a pool's channel; scriptProvisioner by default.
	Provisioner nodeProvisioner
```

New(): после `s.links = newLinkCache(cfg.NodeConfDir)`:
```go
	s.w = newNodeWatch()
	if opt.Provisioner != nil {
		s.prov = opt.Provisioner
	} else {
		s.prov = &scriptProvisioner{cfg: cfg, logf: s.logf, exec: runCmd, rooms: cupsonline.CreateRoomList}
	}
```
(добавить импорт `"github.com/p1neappleXpress/OpenFlux/transport/cupsonline"`).

Serve(): в начале, после `ln, err := net.Listen(...)`:
```go
	s.startNodeWatch(ctx)
```
(`startNodeWatch` не блокирует; создание в горутинах.)

**Step 3.3: `links.go` `profiles()`** — в ветке ошибки:

```go
		link, err := s.links.link(p)
		if err != nil {
			s.logf("pool %s: %v", p.ID, err)
			if p.AutoNode {
				s.start(p)
			}
			continue
		}
```

**Step 3.4: `admin.go` `adminInfo`** — чтобы `opensub status` тоже лечил:

```go
		if _, err := s.links.link(p); err != nil {
			pl.OK, pl.Error = false, err.Error()
			if p.AutoNode {
				s.start(p)
			}
		}
```

**Step 3.5: Запустить тесты**

```
go test ./subscription/server
```
Ожидание: PASS (существующие; новые придут в Task 4).

**Step 3.6: Commit**

```bash
git add subscription/server/watch.go subscription/server/server.go subscription/server/links.go subscription/server/admin.go
git commit -m "subscription: heal autoNode pools on requests and every few minutes"
```

---

### Task 4: Тесты самовосстановления (фейковый провизионер)

**Files:**
- Modify: `subscription/server/server_test.go` (env: поле `prov`, опция в `start`)
- Modify: `subscription/server/heal_test.go` (добавить тесты)

**Step 4.1: `server_test.go`**

`env` struct: добавить поле
```go
	prov  nodeProvisioner
```

`start`:
```go
func (e *env) start(cfg *Config) {
	opt := Options{Now: e.clock.now, Logf: func(string, ...any) {}}
	if e.prov != nil {
		opt.Provisioner = e.prov
	}
	s, err := New(cfg, opt)
	...
}
```

**Step 4.2: `heal_test.go` — фейк и тесты** (файл уже начат в Task 1; дополняем)

```go
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

func (f *fakeProvisioner) Ensure(_ context.Context, p Pool) error {
	f.mu.Lock()
	f.calls++
	f.channel = p.Channel
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
	writeChannel(f.t, nodes, p.Channel, []string{mailruDoc}, "35123")
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
	if c := fake.count(); c > 1 {
		t.Fatalf("during the cooldown calls=%d, want 1", c)
	}
	e.clock.add(4 * time.Minute)
	e.call("GET", "/api/v1/subscription", token, nil)
	if c := fake.count(); c < 2 {
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
```

Импорты `heal_test.go` (полный список): `os`, `path/filepath`, `strings`, `sync`, `testing`, `time`, `errors`, `context`, `github.com/p1neappleXpress/OpenFlux/provision`.

**Step 4.3: Запустить тесты несколько раз (гонки)**

```
go test -race ./subscription/server
```
Ожидание: PASS (5 прогонов подряд).

**Step 4.4: Commit**

```bash
git add subscription/server/server_test.go subscription/server/heal_test.go
git commit -m "subscription/server: autoNode self-healing tests"
```

---

### Task 5: Вся проверка репозитория + гонки

**Step 5.1: `go vet` + все тесты**

```
go vet ./subscription/server
go test ./...
```
Ожидание: `ok` для всех пакетов (Go-клиентский shared-код не зависит от opensub).

**Step 5.2: Race-прогон**

```
go test -race ./subscription/server -count 3
```

**Step 5.3: Commit (если vet что-то поправил)**

```bash
git add -A
git commit -m "subscription/server: vet and race clean" || true
```

---

### Task 6: Деплой на сервер 141.11.209.233 и включение

**Предусловия (проверить руками):**
- `ssh root@141.11.209.233` работает; `ls /opt/openflux-node/node-install.sh` есть (v1.4.1, ядро в `/opt/openflux-node/bin`).
- `opensub` бинарь лежит `/opt/openflux-sub/opensub`; юнит `openflux-sub.service` (User=openflux-sub).

**Step 6.1: Собрать linux-бинарь из OpenFlux**

```
cd C:\Users\meepo\openflux\OpenFlux
$env:GOOS="linux"; $env:GOARCH="amd64"; $env:CGO_ENABLED="0"; go build -o opensub-linux ./cmd/opensub
```
Ожидание: `opensub-linux` в корне репо.

**Step 6.2: Бэкап и правка конфига на сервере**

```
ssh root@141.11.209.233 'cp -a /etc/openflux-sub/config.json /etc/openflux-sub/config.json.bak-2026-10-11'
```
Правка `config.json`: у пула `main` добавить
```json
"autoNode": true,
"transports": [ { "type": "cupsonline" } ]
```
(место рядом с `"channel": "fork-all"`). Проверить `python3 -m json.tool /etc/openflux-sub/config.json`.

**Step 6.3: Юнит — права root для авто-создания каналов**

Создать `/etc/systemd/system/openflux-sub.service.d/auto-node.conf`:
```ini
[Service]
User=root
Group=root
SupplementaryGroups=
ReadWritePaths=/var/lib/openflux-sub /etc/openflux-node /var/lib/openflux-node /opt/openflux-node
```
(всё прочее защитное остаётся). Затем:
```
systemctl daemon-reload
``` 

**Step 6.4: Замена бинаря и рестарт**

```
scp OpenFlux/opensub-linux root@141.11.209.233:/opt/openflux-sub/opensub.new
ssh root@141.11.209.233 'chmod 0755 /opt/openflux-sub/opensub.new && cp -a /opt/openflux-sub/opensub /opt/openflux-sub/opensub.old && mv /opt/openflux-sub/opensub.new /opt/openflux-sub/opensub && systemctl restart openflux-sub && sleep 2 && systemctl status openflux-sub --no-pager'
```
Логи сразу: `journalctl -u openflux-sub -n 20 --no-pager`.

**Step 6.5: Проверка самовосстановления**

```
ssh root@141.11.209.233 'OPENSUB_CONFIG=/etc/openflux-sub/config.json /opt/openflux-sub/opensub status'
```
Ожидание: через ~1-2 мин пул `main` в `ok` (в первый проход канал создаётся), канал `fork-all` появился в `/etc/openflux-node/`; `systemctl status openflux-node@fork-all` — active; `ss -ltn` — новый порт прослушивается. Пример напильника: пока BROKEN — подождать 30-60 с и повторить status.

**Step 6.6: Контрольный запрос подписчика**

Регистрация тестового аккаунта из клиента (или `opensub key-add test1`), затем `GET /api/v1/subscription` у токена: `"status":"active"`, `profiles` содержит `main` (не `null`). Дешифровка: `openflux://` ссылка открывается в `OpenFluxDesktop` (после фикса `coerceInputValues`). Тестовый аккаунт/ключ удалить (`opensub delete-user` / `key-revoke`).

**Step 6.7: Документация**

`docs/subscriptions.ru.md`: короткий раздел «Авто-создание нод» — `autoNode`, `transports` (cupsonline пустой url), права root юнита, `nodeScript`.

```bash
git add docs/subscriptions.ru.md
git commit -m "docs: autoNode pools create their node themselves"
```

---

## Критерии готовности

1. `go test ./...` и `go test -race ./subscription/server -count 3` зелёные.
2. На проде после рестарта `opensub status`: пул `main` ok, `openflux-node@fork-all` active, порт слушается.
3. Контрольный клиент декодирует документ: `profiles` = [main], не `null`.
4. Повторный сбой (удаление `node.conf` канала) сам восстанавливается в пределах ритма `nodeCheckEvery` без действий владельца.