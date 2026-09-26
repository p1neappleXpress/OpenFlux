package provision

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
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
	script := regexp.MustCompile(`(?m)^SHA_amd64=.*$`).
		ReplaceAll(body, []byte(`SHA_amd64="`+os.Getenv("OPENFLUX_TEST_CORE_SHA")+`"`))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write(script) })}
	go srv.Serve(ln)
	defer srv.Close()
	allowPlainScriptURL = true
	good := Script{URL: "http://" + ln.Addr().String() + "/node-install.sh", SHA256: ScriptHash(script)}
	if os.Getenv("OPENFLUX_TEST_PINNED") != "" {
		good = Pinned()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
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
	plan, err := c.Plan(id, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %+v", plan)
	key, _ := NewKey()
	cookies, signedIn, err := CookieStore(os.Getenv("OPENFLUX_TEST_DOC"), "Session_id=test-login-value; spravka=pass")
	if err != nil || !signedIn {
		t.Fatalf("CookieStore: %v %v", signedIn, err)
	}
	ch := Channel{ID: id, URL: os.Getenv("OPENFLUX_TEST_DOC"), Key: key, Port: plan.Port, Cookies: cookies}

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
	if out, _, _ := c.run("stat -c '%a %U' /var/lib/openflux-node/"+id+"/cookies.json", nil); strings.TrimSpace(string(out)) != "600 openflux-node" {
		t.Fatalf("cookies.json: %q", out)
	}
	if out, _, _ := c.run("sudo -S -p '' cat /var/lib/openflux-node/"+id+"/cookies.json", []byte(userPass+"\n")); !strings.Contains(string(out), "test-login-value") {
		t.Fatalf("cookies.json content: %q", out)
	}
	if out, _, _ := c.run("ps -eo args; sudo -S -p '' journalctl -u openflux-node@"+id+" --no-pager", []byte(userPass+"\n")); strings.Contains(string(out), "test-login-value") {
		t.Fatal("the Yandex login leaked into ps or the node's log")
	}
	fresh, _, _ := CookieStore(os.Getenv("OPENFLUX_TEST_DOC"), "Session_id=renewed-login")
	if err := c.SetCookies(id, fresh, userPass); err != nil {
		t.Fatal(err)
	}
	if out, _, _ := c.run("sudo -S -p '' cat /var/lib/openflux-node/"+id+"/cookies.json", []byte(userPass+"\n")); !strings.Contains(string(out), "renewed-login") {
		t.Fatalf("set-cookies did not replace the login: %q", out)
	}
	if _, err := c.Plan(id, 0, true); err == nil {
		t.Fatal("planning an existing channel must fail")
	}
	again, err := c.Plan("other", plan.Port, false)
	if err == nil {
		t.Fatalf("the channel's port must count as taken: %+v", again)
	}
	next, err := c.Plan("other", 0, false)
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
	if err := c.Remove(id, userPass); err != nil {
		t.Fatal(err)
	}
	if out, _, _ := c.run("test -e /etc/openflux-node/"+id+" && echo left", nil); strings.TrimSpace(string(out)) != "" {
		t.Fatal("channel files left after remove")
	}
}
