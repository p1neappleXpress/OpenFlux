package provision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestInstallOnVDS drives the whole install against a real systemd host:
//
//	OPENFLUX_TEST_SSH=127.0.0.1:22          sshd of the test VDS
//	OPENFLUX_TEST_ROOT_PASSWORD=...         root's password
//	OPENFLUX_TEST_USER=deploy:...           a sudoer that needs a password
//	OPENFLUX_TEST_CORE_SHA=...              sha256 of the core preinstalled
//	                                        as /opt/openflux-node/bin/openflux-<ver>
//	OPENFLUX_TEST_DOC=https://docs.yandex.ru/edit/d/...
//	OPENFLUX_TEST_PINNED=1                  use the real pinned script and
//	                                        release core from GitHub instead
//
// The test process must share the VDS's loopback (docker --network
// container:<vds>): the VDS downloads the script from the test's server.
func TestInstallOnVDS(t *testing.T) {
	addr := os.Getenv("OPENFLUX_TEST_SSH")
	if addr == "" {
		t.Skip("OPENFLUX_TEST_SSH not set")
	}
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)
	user, userPass, _ := strings.Cut(os.Getenv("OPENFLUX_TEST_USER"), ":")

	body, err := os.ReadFile("../deploy/node-install.sh")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + ln.Addr().String()
	// The script as a release at version pins core: GitHub is this server.
	release := func(version, core string) []byte {
		s := regexp.MustCompile(`(?m)^SHA_amd64=.*$`).ReplaceAll(body, []byte(`SHA_amd64="`+core+`"`))
		s = regexp.MustCompile(`(?m)^CORE_VERSION=.*$`).ReplaceAll(s, []byte(`CORE_VERSION="`+version+`"`))
		for _, v := range []string{"GITHUB_API", "GITHUB_RAW", "GITHUB_WEB"} {
			s = regexp.MustCompile(`(?m)^`+v+`=.*$`).ReplaceAll(s, []byte(v+`="`+base+`"`))
		}
		return s
	}
	script := release("node-v1.0.1", os.Getenv("OPENFLUX_TEST_CORE_SHA"))
	gh := newFakeReleases(t, os.Getenv("OPENFLUX_TEST_CORE_FILE"), release)
	gh.add("node-v1.0.1", gh.good)
	mux := http.NewServeMux()
	mux.HandleFunc("/node-install.sh", func(w http.ResponseWriter, _ *http.Request) { w.Write(script) })
	// The same script following another repository, like a fork's.
	otherRepo := regexp.MustCompile(`(?m)^RELEASE_REPO=.*$`).ReplaceAll(script, []byte(`RELEASE_REPO="someone/OpenFlux"`))
	mux.HandleFunc("/other-install.sh", func(w http.ResponseWriter, _ *http.Request) { w.Write(otherRepo) })
	mux.Handle("/", gh)
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	defer srv.Close()
	allowPlainScriptURL = true
	good := Script{URL: "http://" + ln.Addr().String() + "/node-install.sh", SHA256: ScriptHash(script)}
	if os.Getenv("OPENFLUX_TEST_PINNED") != "" {
		good = Pinned()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	root := Target{Host: host, Port: port, User: "root", Password: os.Getenv("OPENFLUX_TEST_ROOT_PASSWORD")}
	_, err = Dial(ctx, root)
	var hk *HostKeyError
	if !errors.As(err, &hk) || hk.Mismatch {
		t.Fatalf("first dial: want an untrusted host key, got %v", err)
	}
	root.HostKey = "SHA256:not-it"
	if _, err = Dial(ctx, root); !errors.As(err, &hk) || !hk.Mismatch {
		t.Fatalf("want a host key mismatch, got %v", err)
	}
	root.HostKey = hk.Fingerprint

	wrongPass := root
	wrongPass.Password = "nope"
	if _, err := Dial(ctx, wrongPass); err == nil || errors.As(err, &hk) {
		t.Fatalf("want an auth failure, got %v", err)
	}

	c, err := Dial(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.FetchScript(Script{URL: good.URL, SHA256: strings.Repeat("0", 64)}); err == nil {
		t.Fatal("a script with another hash must be refused")
	}
	if err := c.FetchScript(good); err != nil {
		t.Fatal(err)
	}
	p, err := c.Probe()
	if err != nil || p.Sudo != "root" || !p.Systemd {
		t.Fatalf("root probe: %+v %v", p, err)
	}
	// A core some other installer left, named like a newer release of this
	// repository (an old fork's node-v1.4.0), must not be kept.
	if _, _, err := c.run("cd /opt/openflux-node/bin && cp openflux-node-v1.0.1 openflux-node-v1.4.0 && ln -sfn openflux-node-v1.4.0 openflux", nil); err != nil {
		t.Fatal(err)
	}
	c.Close()

	d := Target{Host: host, Port: port, User: user, Password: userPass, HostKey: root.HostKey}
	c, err = Dial(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.FetchScript(good); err != nil {
		t.Fatal(err)
	}
	if p, err = c.Probe(); err != nil || p.Sudo != "password" {
		t.Fatalf("deploy probe: %+v %v", p, err)
	}
	id, _ := NewChannelID()
	volga := []ChannelTransport{
		{Type: "vyandex", URL: os.Getenv("OPENFLUX_TEST_DOC")},
		{Type: "mailru", URL: "https://cloud.mail.ru/public/AbCd/EfGhIjKlM"},
		{Type: "cupsonline", URL: "WyJyb29tLTEiLCJyb29tLTIiXQ"},
	}
	plan, err := c.Plan(Channel{ID: id, Transports: volga, AutoUpdate: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %+v", plan)
	if plan.Core != "node-v1.0.1" {
		t.Fatalf("plan keeps a core this script did not install: %s", plan.Core)
	}
	if actions := strings.Join(plan.Actions, "\n"); !strings.Contains(actions, "Яндекс Документ, Mail.ru Документ, cups.online и direct") ||
		!strings.Contains(actions, "Включить автообновление") {
		t.Fatalf("plan actions: %s", actions)
	}
	key, _ := NewKey()
	ch := Channel{ID: id, Transports: volga, Key: key, Port: plan.Port, AutoUpdate: true}

	if err := c.Apply(ch, "wrong-password"); !errors.Is(err, ErrSudoPassword) {
		t.Fatalf("wrong sudo password: got %v", err)
	}
	if out, _, _ := c.run("ls /tmp/openflux-node-conf.* 2>/dev/null | wc -l", nil); strings.TrimSpace(string(out)) != "0" {
		t.Fatalf("config temp file left behind after a refused sudo: %s", out)
	}
	if err := c.Apply(ch, userPass); err != nil {
		t.Fatal(err)
	}
	if out, _, _ := c.run("ls /tmp/openflux-node-conf.* 2>/dev/null | wc -l", nil); strings.TrimSpace(string(out)) != "0" {
		t.Fatalf("config temp file left behind: %s", out)
	}
	if out, _, _ := c.run("systemctl is-active openflux-node@"+id, nil); strings.TrimSpace(string(out)) != "active" {
		t.Fatalf("node not active: %s", out)
	}
	if out, _, _ := c.run("ps -eo args | grep -c '[o]penflux --config'", nil); strings.TrimSpace(string(out)) == "0" {
		t.Fatal("no node process")
	}
	if out, _, _ := c.run("ps -eo args", nil); strings.Contains(string(out), key) {
		t.Fatal("channel key visible in the process list")
	}
	if _, err := c.Plan(Channel{ID: id, Transports: volga}); err == nil {
		t.Fatal("planning an existing channel must fail")
	}
	again, err := c.Plan(Channel{ID: "other", Port: plan.Port})
	if err == nil {
		t.Fatalf("the channel's port must count as taken: %+v", again)
	}
	next, err := c.Plan(Channel{ID: "other"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, u := range next.Untouched {
		found = found || u == id
	}
	if !found {
		t.Fatalf("new channel missing from untouched: %+v", next.Untouched)
	}

	sudo := func(cmd string) string {
		out, errb, _ := c.run("sudo -S -p '' "+cmd, []byte(userPass+"\n"))
		return strings.TrimSpace(string(out) + string(errb))
	}
	conf := sudo("cat /etc/openflux-node/" + id + "/node.conf")
	for _, want := range []string{
		"URL = " + os.Getenv("OPENFLUX_TEST_DOC"), "[Transport vyandex]", "[Transport mailru]",
		"URL = https://cloud.mail.ru/public/AbCd/EfGhIjKlM", "[Transport cupsonline]", "URL = WyJyb29tLTEiLCJyb29tLTIiXQ",
		"Listen = 0.0.0.0:" + strconv.Itoa(plan.Port),
	} {
		if !strings.Contains(conf, want) {
			t.Fatalf("node.conf lacks %q:\n%s", want, conf)
		}
	}
	if out := sudo("systemctl is-enabled openflux-node-update.timer; test -x /opt/openflux-node/node-install.sh && echo copy"); out != "enabled\ncopy" {
		t.Fatalf("updater not installed: %q", out)
	}
	if p, err := c.Probe(); err != nil || !p.AutoUpdate {
		t.Fatalf("probe after install: %+v %v", p, err)
	}

	update := func() string { return sudo("sh /opt/openflux-node/node-install.sh update") }
	core := func() string { return sudo("readlink /opt/openflux-node/bin/openflux") }
	if out := update(); !strings.Contains(out, `"updated":false`) || core() != "openflux-node-v1.0.1" {
		t.Fatalf("no newer release: %s, core %s", out, core())
	}
	gh.add("node-v1.1.0", gh.good)
	gh.add("node-v9.0.0-rc1", gh.bad) // not a release tag the updater takes
	gh.prerelease("node-v9.9.9", gh.bad)
	if out := update(); !strings.Contains(out, `"updated":true`) || core() != "openflux-node-v1.1.0" {
		t.Fatalf("update to node-v1.1.0: %s, core %s", out, core())
	}
	if out := sudo("systemctl is-active openflux-node@" + id); out != "active" {
		t.Fatalf("channel after update: %s", out)
	}
	if out := sudo("grep '^CORE_VERSION=' /opt/openflux-node/node-install.sh"); out != `CORE_VERSION="node-v1.1.0"` {
		t.Fatalf("the updater's script copy: %s", out)
	}
	gh.add("node-v1.2.0", gh.bad)
	if out := update(); !strings.Contains(out, "не поднялись") || core() != "openflux-node-v1.1.0" {
		t.Fatalf("a core that crashes must be rolled back: %s, core %s", out, core())
	}
	if out := sudo("systemctl is-active openflux-node@" + id); out != "active" {
		t.Fatalf("channel after rollback: %s", out)
	}
	if out := update(); !strings.Contains(out, `"skipped":"node-v1.2.0"`) {
		t.Fatalf("a rolled back release must not be tried again: %s", out)
	}
	if out := sudo("sh /opt/openflux-node/node-install.sh autoupdate off; systemctl is-enabled openflux-node-update.timer"); !strings.Contains(out, `"autoupdate":false`) {
		t.Fatalf("autoupdate off: %s", out)
	}
	if out := sudo("sh " + c.script + " autoupdate on; systemctl is-enabled openflux-node-update.timer"); !strings.HasSuffix(out, "enabled") ||
		!strings.Contains(out, `"autoupdate":true`) {
		t.Fatalf("autoupdate on: %s", out)
	}
	// An app with an older pinned script must not take the server back.
	older, err := c.Plan(Channel{ID: "other"})
	if err != nil || older.Core != "node-v1.1.0" {
		t.Fatalf("plan after an update: %+v %v", older, err)
	}
	// A script from another repository does not keep this one's release,
	// however new: the server moves to that repository's core.
	pinned := c.script
	if err := c.FetchScript(Script{URL: base + "/other-install.sh", SHA256: ScriptHash(otherRepo)}); err != nil {
		t.Fatal(err)
	}
	switched, err := c.Plan(Channel{ID: "other"})
	if err != nil || switched.Core != "node-v1.0.1" || !strings.Contains(strings.Join(switched.Actions, "\n"), "someone/OpenFlux") {
		t.Fatalf("plan from another repository: %+v %v", switched, err)
	}
	c.script = pinned

	if err := c.Remove(id, userPass); err != nil {
		t.Fatal(err)
	}
	if out, _, _ := c.run("test -e /etc/openflux-node/"+id+" && echo left", nil); strings.TrimSpace(string(out)) != "" {
		t.Fatal("channel files left after remove")
	}
	if out, _, _ := c.run("test -e /etc/systemd/system/openflux-node-update.timer && echo left", nil); strings.TrimSpace(string(out)) != "" {
		t.Fatal("updater left after the last channel was removed")
	}

	// By hand on the server: list, remove one channel by name, uninstall.
	if err := c.FetchScript(good); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"first", "second"} {
		p, err := c.Plan(Channel{ID: name, AutoUpdate: true})
		if err != nil {
			t.Fatal(err)
		}
		k, _ := NewKey()
		if err := c.Apply(Channel{ID: name, Key: k, Port: p.Port, AutoUpdate: true}, userPass); err != nil {
			t.Fatal(err)
		}
	}
	// systemd sets the mode (StateDirectoryMode); without StateDirectory= apply's 0750 stays.
	if out := sudo("stat -c '%U' /var/lib/openflux-node/first"); out != "openflux-node" {
		t.Fatalf("state directory: %q", out)
	}
	if out := sudo("sh /opt/openflux-node/node-install.sh list"); !strings.Contains(out, `"channel":"first","state":"active"`) ||
		!strings.Contains(out, `"channel":"second","state":"active"`) || !strings.Contains(out, `"autoupdate":true`) {
		t.Fatalf("list: %s", out)
	}
	if out := sudo("sh /opt/openflux-node/node-install.sh remove first"); !strings.Contains(out, `"ok":true`) {
		t.Fatalf("remove by name: %s", out)
	}
	if out := sudo(`sh -c 'test -e /etc/openflux-node/first && echo left; systemctl is-active openflux-node@first; systemctl is-active openflux-node@second'`); out != "inactive\nactive" {
		t.Fatalf("after remove first: %q", out)
	}
	if out := sudo("sh /opt/openflux-node/node-install.sh remove nosuch"); !strings.Contains(out, `"ok":false`) {
		t.Fatalf("remove of a missing channel: %s", out)
	}
	if out := sudo("sh /opt/openflux-node/node-install.sh uninstall"); !strings.Contains(out, `"removed":["second"]`) {
		t.Fatalf("uninstall: %s", out)
	}
	// sudo runs one command: the check is a script for sh.
	left := sudo(`sh -c 'for p in /opt/openflux-node /etc/openflux-node /var/lib/openflux-node /etc/systemd/system/openflux-node@.service ` +
		`/etc/systemd/system/openflux-node-update.timer /etc/systemd/system/openflux-node-update.service; do test -e $p && echo $p; done; ` +
		`id openflux-node >/dev/null 2>&1 && echo user; systemctl is-active openflux-node@second; ps -eo args | grep -c "[o]penflux --config"'`)
	if left != "inactive\n0" {
		t.Fatalf("left after uninstall: %q", left)
	}
}

// fakeReleases is GitHub for the updater: the releases listing, each
// release's node-install.sh at its tag, its SHA256SUMS and core.
type fakeReleases struct {
	t         *testing.T
	good, bad []byte
	script    func(version, core string) []byte
	mu        sync.Mutex
	cores     map[string][]byte
	pre       map[string]bool
	order     []string
}

func newFakeReleases(t *testing.T, coreFile string, script func(version, core string) []byte) *fakeReleases {
	f := &fakeReleases{t: t, script: script, cores: map[string][]byte{}, pre: map[string]bool{}}
	f.bad = []byte("#!/bin/sh\nexit 1\n")
	if coreFile != "" {
		b, err := os.ReadFile(coreFile)
		if err != nil {
			t.Fatal(err)
		}
		f.good = b
	}
	return f
}

func (f *fakeReleases) add(tag string, core []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cores[tag] = core
	f.order = append([]string{tag}, f.order...)
}

func (f *fakeReleases) prerelease(tag string, core []byte) {
	f.add(tag, core)
	f.mu.Lock()
	f.pre[tag] = true
	f.mu.Unlock()
}

func (f *fakeReleases) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	const repo = "/p1neappleXpress/OpenFlux/"
	p := r.URL.Path
	switch {
	case p == "/repos"+repo+"releases":
		var list []map[string]interface{}
		for _, tag := range f.order {
			list = append(list, map[string]interface{}{
				"tag_name": tag, "draft": false, "prerelease": f.pre[tag],
				"author": map[string]interface{}{"login": "x", "id": 1}, "assets": []interface{}{},
			})
		}
		json.NewEncoder(w).Encode(list)
	case strings.HasPrefix(p, repo+"releases/download/"):
		tag, file, _ := strings.Cut(strings.TrimPrefix(p, repo+"releases/download/"), "/")
		core, ok := f.cores[tag]
		switch {
		case !ok:
			http.NotFound(w, r)
		case file == "SHA256SUMS":
			fmt.Fprintf(w, "%s  openflux-linux-amd64\n", ScriptHash(core))
		case file == "openflux-linux-amd64":
			w.Write(core)
		default:
			http.NotFound(w, r)
		}
	case strings.HasSuffix(p, "/deploy/node-install.sh"):
		tag := strings.TrimSuffix(strings.TrimPrefix(p, repo), "/deploy/node-install.sh")
		core, ok := f.cores[tag]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(f.script(tag, ScriptHash(core)))
	default:
		http.NotFound(w, r)
	}
}
