package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/p1neappleXpress/OpenFlux/share"
	"github.com/p1neappleXpress/OpenFlux/subscription"
)

const (
	adminToken = "admin-token-for-tests-0123456789"
	mailruDoc  = "https://cloud.mail.ru/public/z5iM/XFh98DW3b"
	boardDoc   = "https://boards.yandex.ru/whiteboard/?hash=11dfe61e84149fa0295bdf27e1136775"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type env struct {
	t     *testing.T
	srv   *Server
	http  *httptest.Server
	clock *clock
	nodes string
	data  string
	prov  nodeProvisioner
}

// writeChannel is a channel as node-install.sh leaves it: documents, direct port, key.
func writeChannel(t *testing.T, dir, name string, docs []string, port string) {
	t.Helper()
	base := filepath.Join(dir, name)
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	conf := "Role = exit\nMode = l4\nURL = " + docs[0] + "\n"
	for _, d := range docs {
		typ := "mailru"
		if strings.Contains(d, "boards.yandex") {
			typ = "boards"
		}
		conf += "\n[Transport " + typ + "]\nType = " + typ + "\nPriority = 90\nURL = " + d + "\n"
	}
	conf += "\n[Transport direct]\nType = direct\nPriority = 50\nListen = 0.0.0.0:" + port + "\n"
	if err := os.WriteFile(filepath.Join(base, "node.conf"), []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("ab", 32)
	if err := os.WriteFile(filepath.Join(base, "encryption-key"), []byte(key+"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
}

func newEnv(t *testing.T, mutate ...func(*Config)) *env {
	t.Helper()
	e := &env{t: t, clock: &clock{t: time.Unix(1_800_000_000, 0)}, nodes: t.TempDir(), data: t.TempDir()}
	writeChannel(t, e.nodes, "fork-all", []string{mailruDoc}, "34443")
	writeChannel(t, e.nodes, "de", []string{boardDoc}, "41000")
	cfg := &Config{
		Listen: ":8443", PublicHost: "203.0.113.5", AdminToken: adminToken, DataDir: e.data, NodeConfDir: e.nodes,
		Title: "Test", Support: "https://t.me/support", AutoTrial: true, BcryptCost: 4,
		Pools: []Pool{{ID: "main", Name: "Главная", Channel: "fork-all"}, {ID: "de", Name: "Германия", Channel: "de", Host: "203.0.113.9"}},
		Plans: []Plan{
			{ID: "trial", Name: "Пробный", Days: 3, Devices: 1, Trial: true},
			{ID: "month", Name: "Месяц", Days: 30, Devices: 3, Price: "299 ₽"},
			{ID: "solo", Name: "Один регион", Days: 30, Devices: 1, Pools: []string{"de"}},
		},
		Payment: Payment{Provider: "manual", Instructions: "Напишите в поддержку"},
	}
	for _, m := range mutate {
		m(cfg)
	}
	e.start(cfg)
	return e
}

func (e *env) start(cfg *Config) {
	opt := Options{Now: e.clock.now, Logf: func(string, ...any) {}}
	if e.prov != nil {
		opt.Provisioner = e.prov
	}
	s, err := New(cfg, opt)
	if err != nil {
		e.t.Fatal(err)
	}
	e.srv = s
	if e.http != nil {
		e.http.Close()
	}
	e.http = httptest.NewServer(s.Handler())
	e.t.Cleanup(e.http.Close)
}

type reply map[string]any

func (r reply) str(k string) string { s, _ := r[k].(string); return s }
func (r reply) num(k string) int64  { f, _ := r[k].(float64); return int64(f) }
func (r reply) obj(k string) reply  { o, _ := r[k].(map[string]any); return o }
func (r reply) list(k string) []any { l, _ := r[k].([]any); return l }

// call sends a request; auth is a bearer token.
func (e *env) call(method, path, auth string, body any) (int, reply) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.http.URL+path, rd)
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out reply
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (e *env) register(login, device string) (token string, r reply) {
	e.t.Helper()
	code, r := e.call("POST", "/api/v1/register", "", map[string]any{"login": login, "password": "correct horse battery", "deviceName": device})
	if code != http.StatusCreated {
		e.t.Fatalf("register %s: %d %v", login, code, r)
	}
	return r.str("deviceToken"), r
}

// idOf finds a user's id by login through /admin/users.
func (e *env) idOf(login string) string {
	e.t.Helper()
	req, _ := http.NewRequest("GET", e.http.URL+"/admin/users", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var users []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&users); err != nil {
		e.t.Fatal(err)
	}
	for _, u := range users {
		if reply(u).str("login") == login {
			return reply(u).str("id")
		}
	}
	e.t.Fatalf("no user %q", login)
	return ""
}

func (e *env) admin(method, path string, body any) (int, reply) {
	e.t.Helper()
	return e.call(method, path, adminToken, body)
}

func TestRegisterGivesTrialAndLocations(t *testing.T) {
	e := newEnv(t)
	token, r := e.register("Ivan@Example.com", "Телефон Ивана")
	if len(r.str("recoveryKey")) != 39 || strings.Count(r.str("recoveryKey"), "-") != 7 {
		t.Fatalf("recovery key %q", r.str("recoveryKey"))
	}
	acc := r.obj("account")
	if acc.str("login") != "ivan@example.com" || acc.str("status") != "active" || acc.obj("plan").str("id") != "trial" {
		t.Fatalf("account: %v", acc)
	}
	if want := e.clock.now().Add(3 * 24 * time.Hour).Unix(); acc.num("expires") != want {
		t.Fatalf("expires %d, want %d", acc.num("expires"), want)
	}
	if acc["trialAvailable"] != false || len(acc["devices"].([]any)) != 1 {
		t.Fatalf("account: %v", acc)
	}

	code, doc := e.call("GET", "/api/v1/subscription", token, nil)
	if code != 200 || doc.str("status") != "active" || doc.str("title") != "Test" || doc.str("support") != "https://t.me/support" {
		t.Fatalf("document: %d %v", code, doc)
	}
	profiles := doc.list("profiles")
	if len(profiles) != 2 {
		t.Fatalf("profiles: %v", profiles)
	}
	// Every link is a real openflux:// link, with the channel's key, port and context.
	byID := map[string]reply{}
	for _, p := range profiles {
		pr := reply(p.(map[string]any))
		byID[pr.str("id")] = pr
	}
	cfg, err := share.Decode(byID["main"].str("link"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Secret != strings.Repeat("ab", 32) || cfg.Context != mailruDoc || !cfg.Negotiate || byID["main"].str("name") != "Главная" {
		t.Fatalf("main link: %+v", cfg)
	}
	var direct string
	for _, tr := range cfg.Transports {
		if tr.Type == "direct" {
			direct = tr.Dial
		}
	}
	if direct != "203.0.113.5:34443" {
		t.Fatalf("direct dial %q", direct)
	}
	de, _ := share.Decode(byID["de"].str("link"))
	if de.Transports[len(de.Transports)-1].Dial != "203.0.113.9:41000" || de.Transports[0].Type != "boards" {
		t.Fatalf("de link: %+v", de)
	}

	// The shared document check passes it as the apps will.
	var d subscription.Document
	raw, _ := json.Marshal(doc)
	json.Unmarshal(raw, &d)
	if skipped, err := d.Check(); err != nil || skipped != 0 {
		t.Fatalf("Check: skipped %d, %v", skipped, err)
	}
}

func TestRegisterRules(t *testing.T) {
	e := newEnv(t)
	e.register("ivan", "a")
	for name, body := range map[string]map[string]any{
		"taken":    {"login": "IVAN", "password": "another long password"},
		"short":    {"login": "petr", "password": "short"},
		"samepass": {"login": "petr-petr-petr", "password": "petr-petr-petr"},
		"badlogin": {"login": "a b", "password": "another long password"},
	} {
		code, r := e.call("POST", "/api/v1/register", "", body)
		if code < 400 || r.str("error") == "" || r.str("message") == "" {
			t.Fatalf("%s: %d %v", name, code, r)
		}
	}
	if code, r := e.call("POST", "/api/v1/register", "", map[string]any{"login": "IVAN", "password": "another long password"}); code != 409 || r.str("error") != "login_taken" {
		t.Fatalf("taken: %d %v", code, r)
	}
}

func TestLoginLimitsAndSameAnswer(t *testing.T) {
	e := newEnv(t)
	e.register("ivan", "phone")
	bad := func(login string) (int, reply) {
		return e.call("POST", "/api/v1/login", "", map[string]any{"login": login, "password": "wrong password!!"})
	}
	c1, r1 := bad("ivan")
	c2, r2 := bad("nobody")
	if c1 != 401 || c2 != 401 || r1.str("error") != "invalid_credentials" || r1.str("message") != r2.str("message") {
		t.Fatalf("a wrong password and an unknown login must look alike: %d %v / %d %v", c1, r1, c2, r2)
	}
	for i := 0; i < 4; i++ {
		bad("ivan")
	}
	// The sixth try is refused even with the right password.
	code, r := e.call("POST", "/api/v1/login", "", map[string]any{"login": "ivan", "password": "correct horse battery"})
	if code != 429 || r.str("error") != "rate_limited" {
		t.Fatalf("locked: %d %v", code, r)
	}
	e.clock.add(16 * time.Minute)
	code, r = e.call("POST", "/api/v1/login", "", map[string]any{"login": "ivan", "password": "correct horse battery", "deviceName": "x"})
	if code != 409 || r.str("error") != "device_limit" {
		// the trial plan allows one device: the limit answer proves the password was accepted
		t.Fatalf("after the window: %d %v", code, r)
	}
}

func TestDeviceLimitAndReplace(t *testing.T) {
	e := newEnv(t)
	first, r := e.register("ivan", "Телефон")
	firstID := r.str("deviceId")
	login := func(extra map[string]any) (int, reply) {
		b := map[string]any{"login": "ivan", "password": "correct horse battery", "deviceName": "Ноутбук"}
		for k, v := range extra {
			b[k] = v
		}
		return e.call("POST", "/api/v1/login", "", b)
	}
	code, r := login(nil)
	if code != 409 || r.str("error") != "device_limit" || len(r.list("devices")) != 1 {
		t.Fatalf("limit: %d %v", code, r)
	}
	if reply(r.list("devices")[0].(map[string]any)).str("name") != "Телефон" {
		t.Fatalf("devices: %v", r["devices"])
	}
	code, r = login(map[string]any{"replaceDeviceId": firstID})
	if code != 200 {
		t.Fatalf("replace: %d %v", code, r)
	}
	if code, _ := e.call("GET", "/api/v1/account", first, nil); code != 401 {
		t.Fatalf("the replaced device keeps working: %d", code)
	}
	if code, acc := e.call("GET", "/api/v1/account", r.str("deviceToken"), nil); code != 200 || acc.str("login") != "ivan" {
		t.Fatalf("new device: %d %v", code, acc)
	}
}

func TestLogoutAndRemoveDevice(t *testing.T) {
	e := newEnv(t)
	e.admin("POST", "/admin/users/ivan/grant", nil) // no such user yet: 404, harmless
	token, _ := e.register("ivan", "a")
	if code, _ := e.admin("POST", "/admin/users/ivan/grant", map[string]any{"planId": "month"}); code != 200 {
		t.Fatalf("grant: %d", code)
	}
	code, r := e.call("POST", "/api/v1/login", "", map[string]any{"login": "ivan", "password": "correct horse battery", "deviceName": "b"})
	if code != 200 {
		t.Fatalf("second device: %d %v", code, r)
	}
	second, secondID := r.str("deviceToken"), r.str("deviceId")
	if code, acc := e.call("DELETE", "/api/v1/devices/"+secondID, token, nil); code != 200 || len(acc.list("devices")) != 1 {
		t.Fatalf("remove: %d %v", code, acc)
	}
	if code, _ := e.call("GET", "/api/v1/account", second, nil); code != 401 {
		t.Fatalf("removed device: %d", code)
	}
	if code, _ := e.call("POST", "/api/v1/logout", token, nil); code != 200 {
		t.Fatalf("logout: %d", code)
	}
	if code, _ := e.call("GET", "/api/v1/subscription", token, nil); code != 401 {
		t.Fatalf("after logout: %d", code)
	}
	if code, _ := e.call("GET", "/api/v1/subscription", "", nil); code != 401 {
		t.Fatalf("no token: %d", code)
	}
}

func TestRecoveryReplacesPasswordKeyAndDevices(t *testing.T) {
	e := newEnv(t)
	token, r := e.register("ivan", "phone")
	key := r.str("recoveryKey")
	body := func(k string) map[string]any {
		return map[string]any{"login": "ivan", "recoveryKey": k, "newPassword": "brand new password", "deviceName": "new"}
	}
	if code, _ := e.call("POST", "/api/v1/recover", "", body("AAAA-BBBB-CCCC-DDDD-EEEE-FFFF-GGGG-HHHH")); code != 401 {
		t.Fatalf("wrong key: %d", code)
	}
	code, r := e.call("POST", "/api/v1/recover", "", body(strings.ToLower(strings.ReplaceAll(key, "-", " "))))
	if code != 200 || r.str("recoveryKey") == "" || r.str("recoveryKey") == key {
		t.Fatalf("recover: %d %v", code, r)
	}
	if code, _ := e.call("GET", "/api/v1/account", token, nil); code != 401 {
		t.Fatalf("an old device survived recovery: %d", code)
	}
	if code, _ := e.call("POST", "/api/v1/recover", "", body(key)); code != 401 {
		t.Fatalf("the old recovery key still works: %d", code)
	}
	if code, _ := e.call("POST", "/api/v1/login", "", map[string]any{"login": "ivan", "password": "correct horse battery"}); code != 401 {
		t.Fatalf("the old password still works: %d", code)
	}
}

func TestPasswordChangeSignsOutOthers(t *testing.T) {
	e := newEnv(t)
	token, _ := e.register("ivan", "a")
	e.admin("POST", "/admin/users/ivan/grant", map[string]any{"planId": "month"})
	_, r := e.call("POST", "/api/v1/login", "", map[string]any{"login": "ivan", "password": "correct horse battery", "deviceName": "b"})
	other := r.str("deviceToken")
	if code, _ := e.call("POST", "/api/v1/password", token, map[string]any{"password": "wrong", "newPassword": "a brand new password"}); code != 401 {
		t.Fatalf("wrong old password: %d", code)
	}
	if code, _ := e.call("POST", "/api/v1/password", token, map[string]any{"password": "correct horse battery", "newPassword": "short"}); code != 400 {
		t.Fatalf("weak new password: %d", code)
	}
	if code, acc := e.call("POST", "/api/v1/password", token, map[string]any{"password": "correct horse battery", "newPassword": "a brand new password"}); code != 200 || len(acc.list("devices")) != 1 {
		t.Fatalf("change: %d %v", code, acc)
	}
	if code, _ := e.call("GET", "/api/v1/account", other, nil); code != 401 {
		t.Fatalf("another device survived the change: %d", code)
	}
	if code, _ := e.call("GET", "/api/v1/account", token, nil); code != 200 {
		t.Fatalf("this device was signed out: %d", code)
	}
}

func TestExpiryPurchaseAndPlans(t *testing.T) {
	e := newEnv(t)
	token, _ := e.register("ivan", "phone")

	code, plans := e.call("GET", "/api/v1/plans", "", nil)
	if code != 200 || len(plans.list("plans")) != 2 || plans.obj("payment").str("provider") != "manual" {
		t.Fatalf("plans: %d %v", code, plans)
	}
	for _, p := range plans.list("plans") {
		if reply(p.(map[string]any)).str("id") == "trial" {
			t.Fatal("the trial plan is not for sale")
		}
	}

	e.clock.add(3*24*time.Hour + time.Minute)
	_, doc := e.call("GET", "/api/v1/subscription", token, nil)
	if doc.str("status") != "expired" || len(doc.list("profiles")) != 0 {
		t.Fatalf("expired: %v", doc)
	}
	_, acc := e.call("GET", "/api/v1/account", token, nil)
	if acc.str("status") != "expired" || acc["trialAvailable"] != false {
		t.Fatalf("account: %v", acc)
	}

	if code, _ := e.call("POST", "/api/v1/checkout", token, map[string]any{"planId": "trial"}); code != 404 {
		t.Fatalf("buying the trial: %d", code)
	}
	code, order := e.call("POST", "/api/v1/checkout", token, map[string]any{"planId": "month"})
	if code != 200 || order.str("status") != "pending" || order.str("instructions") != "Напишите в поддержку" {
		t.Fatalf("checkout: %d %v", code, order)
	}
	_, again := e.call("POST", "/api/v1/checkout", token, map[string]any{"planId": "month"})
	if again.str("orderId") != order.str("orderId") {
		t.Fatal("asking again opened a second order")
	}

	// Not paid yet: nothing changes. The owner (or a bot) confirms.
	_, st := e.call("GET", "/api/v1/orders/"+order.str("orderId"), token, nil)
	if st.str("status") != "pending" {
		t.Fatalf("order: %v", st)
	}
	if code, _ := e.call("POST", "/admin/orders/"+order.str("orderId")+"/paid", token, nil); code != 401 {
		t.Fatalf("a device token must not confirm payments: %d", code)
	}
	if code, r := e.admin("POST", "/admin/orders/"+order.str("orderId")+"/paid", nil); code != 200 || r.str("status") != "paid" {
		t.Fatalf("paid: %d %v", code, r)
	}
	e.admin("POST", "/admin/orders/"+order.str("orderId")+"/paid", nil) // idempotent
	want := e.clock.now().Add(30 * 24 * time.Hour).Unix()
	_, doc = e.call("GET", "/api/v1/subscription", token, nil)
	if doc.str("status") != "active" || doc.num("expires") != want || len(doc.list("profiles")) != 2 {
		t.Fatalf("after payment: %v (want expires %d)", doc, want)
	}
	_, st = e.call("GET", "/api/v1/orders/"+order.str("orderId"), token, nil)
	if st.str("status") != "paid" {
		t.Fatalf("order: %v", st)
	}

	// Buying again adds to the end, not to now.
	_, o2 := e.call("POST", "/api/v1/checkout", token, map[string]any{"planId": "month"})
	e.admin("POST", "/admin/orders/"+o2.str("orderId")+"/paid", nil)
	_, acc = e.call("GET", "/api/v1/account", token, nil)
	if acc.num("expires") != want+30*24*3600 || acc.num("deviceLimit") != 3 {
		t.Fatalf("second purchase: %v", acc)
	}
}

func TestPlanLimitsLocations(t *testing.T) {
	e := newEnv(t)
	token, _ := e.register("ivan", "a")
	e.admin("POST", "/admin/users/ivan/grant", map[string]any{"planId": "solo"})
	_, doc := e.call("GET", "/api/v1/subscription", token, nil)
	ps := doc.list("profiles")
	if len(ps) != 1 || reply(ps[0].(map[string]any)).str("id") != "de" {
		t.Fatalf("solo plan: %v", ps)
	}
}

func TestTrialNeedsAskingWhenNotAutomatic(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.AutoTrial = false })
	token, r := e.register("ivan", "a")
	acc := r.obj("account")
	if acc.str("status") != "expired" || acc["trialAvailable"] != true {
		t.Fatalf("account: %v", acc)
	}
	code, acc := e.call("POST", "/api/v1/trial", token, nil)
	if code != 200 || acc.str("status") != "active" || acc["trialAvailable"] != false {
		t.Fatalf("trial: %d %v", code, acc)
	}
	if code, r := e.call("POST", "/api/v1/trial", token, nil); code != 409 || r.str("error") != "no_trial" {
		t.Fatalf("a second trial: %d %v", code, r)
	}
}

func TestBanRevokesEverything(t *testing.T) {
	e := newEnv(t)
	token, _ := e.register("ivan", "a")
	if code, _ := e.admin("POST", "/admin/users/ivan/ban", map[string]any{"banned": true}); code != 200 {
		t.Fatalf("ban: %d", code)
	}
	if code, _ := e.call("GET", "/api/v1/subscription", token, nil); code != 401 {
		t.Fatalf("a banned account's device: %d", code)
	}
	if code, r := e.call("POST", "/api/v1/login", "", map[string]any{"login": "ivan", "password": "correct horse battery"}); code != 403 || r.str("error") != "banned" {
		t.Fatalf("login: %d %v", code, r)
	}
	e.admin("POST", "/admin/users/ivan/ban", map[string]any{"banned": false})
	if code, _ := e.call("POST", "/api/v1/login", "", map[string]any{"login": "ivan", "password": "correct horse battery"}); code != 200 && code != 409 {
		t.Fatalf("after unban: %d", code)
	}
}

func TestKeysWithoutAccount(t *testing.T) {
	e := newEnv(t)
	code, k := e.admin("POST", "/admin/keys", map[string]any{"name": "Подарок", "days": 7, "pools": []string{"de"}})
	if code != 201 || !strings.HasPrefix(k.str("invite"), "openflux://sub?") || !strings.HasPrefix(k.str("url"), "https://203.0.113.5:8443/sub/") {
		t.Fatalf("add key: %d %v", code, k)
	}
	inv, err := subscription.ParseInvite(k.str("invite"))
	if err != nil || inv.Pin != e.srv.Pin() || inv.Name != "Подарок" {
		t.Fatalf("invite: %+v %v", inv, err)
	}
	token := k.str("token")
	code, doc := e.call("GET", "/sub/"+token, "", nil)
	if code != 200 || doc.str("status") != "active" || len(doc.list("profiles")) != 1 {
		t.Fatalf("key document: %d %v", code, doc)
	}
	if code, _ := e.call("GET", "/sub/not-a-key", "", nil); code != 404 {
		t.Fatalf("unknown key: %d", code)
	}
	e.clock.add(8 * 24 * time.Hour)
	if _, doc := e.call("GET", "/sub/"+token, "", nil); doc.str("status") != "expired" {
		t.Fatalf("expired key: %v", doc)
	}
	if code, _ := e.admin("POST", "/admin/keys/"+k.str("id")+"/extend", map[string]any{"days": 5}); code != 200 {
		t.Fatalf("extend: %d", code)
	}
	if _, doc := e.call("GET", "/sub/"+token, "", nil); doc.str("status") != "active" {
		t.Fatalf("extended key: %v", doc)
	}
	e.admin("DELETE", "/admin/keys/"+k.str("id"), nil)
	if _, doc := e.call("GET", "/sub/"+token, "", nil); doc.str("status") != "revoked" || len(doc.list("profiles")) != 0 {
		t.Fatalf("revoked key: %v", doc)
	}
	// A key that never expires, for all locations.
	_, k2 := e.admin("POST", "/admin/keys", map[string]any{"name": "Вечный"})
	if _, doc := e.call("GET", "/sub/"+k2.str("token"), "", nil); doc.str("status") != "active" || len(doc.list("profiles")) != 2 || doc.num("expires") != 0 {
		t.Fatalf("forever key: %v", doc)
	}
}

func TestAdminNeedsItsToken(t *testing.T) {
	e := newEnv(t)
	token, _ := e.register("ivan", "a")
	for _, tok := range []string{"", "wrong", token} {
		if code, _ := e.call("GET", "/admin/users", tok, nil); code != 401 {
			t.Fatalf("admin with %q: %d", tok, code)
		}
	}
	code, users := e.admin("GET", "/admin/info", nil)
	if code != 200 || users.str("pin") == "" || len(users.list("pools")) != 2 {
		t.Fatalf("info: %d %v", code, users)
	}
}

func TestAdminResetPasswordAndDelete(t *testing.T) {
	e := newEnv(t)
	token, _ := e.register("ivan", "a")
	code, r := e.admin("POST", "/admin/users/ivan/reset-password", nil)
	if code != 200 || r.str("password") == "" {
		t.Fatalf("reset: %d %v", code, r)
	}
	if code, _ := e.call("GET", "/api/v1/account", token, nil); code != 401 {
		t.Fatalf("a device survived the reset: %d", code)
	}
	if code, _ := e.call("POST", "/api/v1/login", "", map[string]any{"login": "ivan", "password": r.str("password"), "deviceName": "x"}); code != 200 {
		t.Fatalf("login with the temporary password: %d", code)
	}
	if code, _ := e.admin("DELETE", "/admin/users/ivan", nil); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	if code, _ := e.call("POST", "/api/v1/login", "", map[string]any{"login": "ivan", "password": r.str("password")}); code != 401 {
		t.Fatalf("a deleted account signs in: %d", code)
	}
}

func TestDeleteOwnAccount(t *testing.T) {
	e := newEnv(t)
	token, _ := e.register("ivan", "a")
	e.call("POST", "/api/v1/checkout", token, map[string]any{"planId": "month"})
	if code, _ := e.call("DELETE", "/api/v1/account", token, map[string]any{"password": "wrong password!!"}); code != 401 {
		t.Fatalf("wrong password: %d", code)
	}
	if code, _ := e.call("POST", "/api/v1/account/delete", token, map[string]any{"password": "wrong password!!"}); code != 401 {
		t.Fatalf("wrong password through POST: %d", code)
	}
	if code, _ := e.call("POST", "/api/v1/account/delete", token, map[string]any{"password": "correct horse battery"}); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	if code, _ := e.call("GET", "/api/v1/account", token, nil); code != 401 {
		t.Fatalf("after delete: %d", code)
	}
	if _, orders := e.admin("GET", "/admin/orders", nil); len(orders) != 0 {
		_ = orders
	}
	// The login can be taken again.
	e.register("ivan", "new")
}

func TestStatePersists(t *testing.T) {
	e := newEnv(t)
	token, _ := e.register("ivan", "a")
	pin := e.srv.Pin()
	e.start(e.srv.cfg)
	if code, acc := e.call("GET", "/api/v1/account", token, nil); code != 200 || acc.str("login") != "ivan" {
		t.Fatalf("after restart: %d %v", code, acc)
	}
	if e.srv.Pin() != pin {
		t.Fatal("the certificate changed on restart: every app's pin would break")
	}
	st, _ := os.Stat(filepath.Join(e.data, "state.json"))
	if st.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Fatalf("state.json is readable by others: %v", st.Mode())
	}
	b, _ := os.ReadFile(filepath.Join(e.data, "state.json"))
	if strings.Contains(string(b), token) || strings.Contains(string(b), "correct horse") {
		t.Fatal("a token or a password is in the state file in the clear")
	}
}

// What the owner does in the app to a channel reaches subscribers: new documents
// in node.conf, the same context.
func TestLinkFollowsTheChannel(t *testing.T) {
	e := newEnv(t)
	token, _ := e.register("ivan", "a")
	link := func() share.Config {
		_, doc := e.call("GET", "/api/v1/subscription", token, nil)
		c, err := share.Decode(reply(doc.list("profiles")[0].(map[string]any)).str("link"))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	before := link()
	writeChannel(t, e.nodes, "fork-all", []string{boardDoc, mailruDoc}, "34443")
	// Same contents age, newer file: the cache must look at the modification time.
	future := time.Now().Add(time.Minute)
	os.Chtimes(filepath.Join(e.nodes, "fork-all", "node.conf"), future, future)
	after := link()
	if len(after.Transports) != len(before.Transports)+1 {
		t.Fatalf("transports before %d, after %d: the change did not arrive", len(before.Transports), len(after.Transports))
	}
	if after.Context != boardDoc {
		t.Fatalf("context %q", after.Context)
	}
}

func TestBrokenPoolIsLeftOut(t *testing.T) {
	e := newEnv(t)
	token, _ := e.register("ivan", "a")
	os.Remove(filepath.Join(e.nodes, "de", "encryption-key"))
	code, doc := e.call("GET", "/api/v1/subscription", token, nil)
	if code != 200 || len(doc.list("profiles")) != 1 {
		t.Fatalf("one broken pool must not break the others: %d %v", code, doc)
	}
}

// Every pool unusable must not turn into "profiles":null, which apps cannot
// decode: the answer keeps the empty list the document always carries.
func TestAllPoolsBrokenGivesEmptyProfilesNotNull(t *testing.T) {
	e := newEnv(t)
	token, _ := e.register("ivan", "a")
	for _, ch := range []string{"fork-all", "de"} {
		if err := os.Remove(filepath.Join(e.nodes, ch, "node.conf")); err != nil {
			t.Fatal(err)
		}
	}
	code, doc := e.call("GET", "/api/v1/subscription", token, nil)
	if code != 200 || doc.str("status") != "active" {
		t.Fatalf("document: %d %v", code, doc)
	}
	if doc["profiles"] == nil {
		t.Fatalf(`an active subscription with no usable pool must answer "profiles":[], not null: %v`, doc)
	}
}

func TestConfigChecks(t *testing.T) {
	good := func() *Config {
		return &Config{
			PublicHost: "h", AdminToken: strings.Repeat("x", 24),
			Pools: []Pool{{ID: "a", Name: "A", Channel: "c"}}, Plans: []Plan{{ID: "p", Name: "P", Days: 1, Devices: 1}},
		}
	}
	if err := good().Check(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Config){
		"no host":        func(c *Config) { c.PublicHost = "" },
		"short token":    func(c *Config) { c.AdminToken = "short" },
		"no pools":       func(c *Config) { c.Pools = nil },
		"bad pool id":    func(c *Config) { c.Pools[0].ID = "A B" },
		"no plans":       func(c *Config) { c.Plans = nil },
		"zero devices":   func(c *Config) { c.Plans[0].Devices = 0 },
		"unknown pool":   func(c *Config) { c.Plans[0].Pools = []string{"zzz"} },
		"stripe":         func(c *Config) { c.Payment.Provider = "stripe" },
		"duplicate pool": func(c *Config) { c.Pools = append(c.Pools, c.Pools[0]) },
	} {
		c := good()
		mutate(c)
		if err := c.Check(); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// Even with a device still signed in (a state ban never leaves, but the check is the last line of defence),
// a banned account is refused.
func TestBannedAccountIsRefusedEvenWithADevice(t *testing.T) {
	e := newEnv(t)
	token, _ := e.register("ivan", "a")
	e.srv.st.mu.Lock()
	e.srv.st.userByLogin("ivan").Banned = true
	e.srv.st.mu.Unlock()
	if code, r := e.call("GET", "/api/v1/subscription", token, nil); code != 403 || r.str("error") != "banned" {
		t.Fatalf("banned with a device: %d %v", code, r)
	}
}
