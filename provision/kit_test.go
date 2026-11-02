package provision

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const kitKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
const kitPub = "ab0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcd"

// fakeInstaller stands in for node-install.sh: it records what it was asked to do and answers as the real one
// does (one JSON line). FAKE_STATE is a directory, FAKE_HAVE the channel it says is installed.
const fakeInstaller = `#!/bin/sh
cmd=$1
echo "$cmd" >> "$FAKE_STATE/calls"
case "$cmd" in
  list)
    if [ -n "${FAKE_HAVE:-}" ]; then printf '[{"channel":"%s","state":"active","port":41000}]\n' "$FAKE_HAVE"; else echo '[]'; fi ;;
  plan)
    cat > "$FAKE_STATE/plan.conf"
    echo '{"ok":true,"channel":"of-kit","port":43210,"arch":"amd64","core":"node-v0","actions":[],"untouched":[]}' ;;
  apply)
    cp "$2" "$FAKE_STATE/apply.conf"
    echo '{"ok":true,"channel":"of-kit","port":43210}' ;;
  set-transports)
    cp "$2" "$FAKE_STATE/set.conf"
    echo '{"ok":true,"channel":"of-kit","state":"active"}' ;;
  *) echo '{"ok":false,"error":"unknown"}' ;;
esac
`

func kitChannel(t *testing.T) Channel {
	t.Helper()
	return Channel{
		ID:  "of-kit",
		Key: kitKey,
		Transports: []ChannelTransport{
			{Type: "mailru", URL: "https://cloud.mail.ru/public/aaaa/bbbb"},
			{Type: "script", Script: "mx", Pubkey: kitPub, URL: "", Params: "eyJ0b2tlbiI6IkIifQ", Package: base64.StdEncoding.EncodeToString([]byte(strings.Repeat("flux-package-", 900)))},
		},
		AutoUpdate: true,
	}
}

// serveInstaller serves body at a loopback address and returns the script that points at it.
func serveInstaller(t *testing.T, body string) Script {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
	t.Cleanup(srv.Close)
	allowPlainScriptURL = true
	t.Cleanup(func() { allowPlainScriptURL = false })
	sum := sha256.Sum256([]byte(body))
	return Script{URL: srv.URL + "/node-install.sh", SHA256: hex.EncodeToString(sum[:])}
}

// shells are the POSIX shells the kit is run under here: sh, and dash where there is one (a Debian's /bin/sh, the
// strictest the nodes see).
func shells() []string {
	out := []string{"sh"}
	if p, err := exec.LookPath("dash"); err == nil {
		out = append(out, p)
	}
	return out
}

// eachShell runs fn as a subtest for every shell.
func eachShell(t *testing.T, fn func(t *testing.T, shell string)) {
	t.Helper()
	for _, sh := range shells() {
		sh := sh
		t.Run(filepath.Base(sh), func(t *testing.T) { fn(t, sh) })
	}
}

func runKit(t *testing.T, shell string, kit *Kit, have string) (state string, out string, err error) {
	t.Helper()
	state = t.TempDir()
	path := filepath.Join(t.TempDir(), "openflux-node.sh")
	if err := os.WriteFile(path, []byte(kit.Script), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(shell, path)
	cmd.Env = append(os.Environ(), "FAKE_STATE="+state, "FAKE_HAVE="+have)
	b, err := cmd.CombinedOutput()
	return state, string(b), err
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestKitIsValidShellAndNamesItsPin(t *testing.T) {
	ch := kitChannel(t)
	kit, err := RenderKit(ch, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, shell := range shells() {
		for name, text := range map[string]string{"script": kit.Script, "paste": kit.Paste} {
			cmd := exec.Command(shell, "-n")
			cmd.Stdin = strings.NewReader(text)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%s does not parse under %s: %v\n%s", name, shell, err, out)
			}
		}
	}
	pin := Pinned()
	if !strings.Contains(kit.Script, pin.URL) || !strings.Contains(kit.Script, pin.SHA256) {
		t.Error("the kit does not carry the build's pinned installer")
	}
	if !strings.Contains(kit.Script, "id -u") {
		t.Error("the kit must check for root")
	}
	if kit.Installer != pin {
		t.Errorf("Installer = %+v", kit.Installer)
	}
	// the longest line is short enough for a terminal (a tty line holds 4095 bytes)
	for _, line := range strings.Split(kit.Paste, "\n") {
		if len(line) > 3000 {
			t.Fatalf("a line of %d bytes will not survive a paste", len(line))
		}
	}
	if !strings.HasPrefix(kit.Paste, "sudo sh <<'OPENFLUX_NODE_KIT'\n") || !strings.HasSuffix(kit.Paste, "\nOPENFLUX_NODE_KIT\n") {
		t.Error("the paste form is a here-document for sudo sh")
	}
	official, err := RenderKit(ch, SourceOfficial)
	if err != nil || !strings.Contains(official.Script, OfficialSHA256) {
		t.Errorf("official source: %v", err)
	}
	if _, err := RenderKit(ch, "nonsense"); err == nil {
		t.Error("an unknown source was accepted")
	}
}

func TestKitCarriersAreTheClientsProfile(t *testing.T) {
	kit, err := renderKit(kitChannel(t), Pinned(), kitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := []KitCarrier{
		{Type: "mailru", URL: "https://cloud.mail.ru/public/aaaa/bbbb", Priority: 90},
		{Type: "script", URL: "", Priority: 60, Script: "mx"},
	}
	if len(kit.Transports) != len(want) {
		t.Fatalf("transports = %+v", kit.Transports)
	}
	for i := range want {
		if kit.Transports[i] != want[i] {
			t.Errorf("transport %d = %+v, want %+v", i, kit.Transports[i], want[i])
		}
	}
	if kit.Context != "openflux_" {
		t.Errorf("context = %q: the fixed placeholder, as the installer derives it", kit.Context)
	}
	// MAX alone has no document: the placeholder both ends derive
	only := kitChannel(t)
	only.Transports = only.Transports[1:]
	k2, err := renderKit(only, Pinned(), kitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if k2.Context == "" {
		t.Error("a channel without a document still has the shared default context")
	}
	if k2.Context != SessionContext(only.Transports) {
		t.Error("the context is the core's rule, not the kit's own")
	}
}

func TestKitRunInstallsANewChannelWithThePlansPort(t *testing.T) {
	eachShell(t, func(t *testing.T, shell string) {
		ch := kitChannel(t)
		pin := serveInstaller(t, fakeInstaller)
		kit, err := renderKit(ch, pin, kitOptions{skipRootCheck: true})
		if err != nil {
			t.Fatal(err)
		}
		state, out, err := runKit(t, shell, kit, "")
		if err != nil {
			t.Fatalf("kit failed: %v\n%s", err, out)
		}
		if got := strings.Fields(readFile(t, filepath.Join(state, "calls"))); strings.Join(got, ",") != "list,plan,apply" {
			t.Fatalf("installer calls = %v, want list, plan, apply", got)
		}
		conf := readFile(t, filepath.Join(state, "apply.conf"))
		for _, line := range []string{"channel=of-kit", "mailru=https://cloud.mail.ru/public/aaaa/bbbb", "autoupdate=yes", "port=43210", "key=" + kitKey} {
			if !containsLine(conf, line) {
				t.Errorf("the applied config lacks %q:\n%s", line, shorten(conf))
			}
		}
		// the package came back whole, on one line, as the installer reads it
		var scriptLine string
		for _, l := range strings.Split(conf, "\n") {
			if strings.HasPrefix(l, "script=") {
				scriptLine = l
			}
		}
		parts := strings.SplitN(strings.TrimPrefix(scriptLine, "script="), ",", 5)
		if len(parts) != 5 || parts[0] != "mx" || parts[1] != kitPub || parts[3] != "eyJ0b2tlbiI6IkIifQ" || parts[4] != ch.Transports[1].Package {
			t.Fatalf("script line = %q ... (%d parts)", shorten(scriptLine), len(parts))
		}
		// the plan saw the config without the secrets and without a port
		plan := readFile(t, filepath.Join(state, "plan.conf"))
		if strings.Contains(plan, "key=") || strings.Contains(plan, "port=") {
			t.Errorf("plan config carries secrets or a port:\n%s", shorten(plan))
		}
		if !strings.Contains(out, "43210") {
			t.Errorf("the person is not told the port:\n%s", out)
		}
	})
}

// The paste form feeds the script to sh on its stdin: nothing the script runs may read (and eat) the rest of it.
func TestKitPasteFormRunsAsAHereDocument(t *testing.T) {
	eachShell(t, func(t *testing.T, shell string) {
		pin := serveInstaller(t, fakeInstaller)
		kit, err := renderKit(kitChannel(t), pin, kitOptions{skipRootCheck: true})
		if err != nil {
			t.Fatal(err)
		}
		state := t.TempDir()
		// sudo is for the person's terminal; here the shell is the test's own
		cmd := exec.Command(shell)
		cmd.Stdin = strings.NewReader(strings.Replace(kit.Paste, "sudo sh <<", shell+" <<", 1))
		cmd.Env = append(os.Environ(), "FAKE_STATE="+state, "FAKE_HAVE=")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("paste form failed: %v\n%s", err, out)
		}
		if got := strings.Fields(readFile(t, filepath.Join(state, "calls"))); strings.Join(got, ",") != "list,plan,apply" {
			t.Fatalf("installer calls = %v\n%s", got, out)
		}
		if !strings.Contains(string(out), "43210") {
			t.Errorf("the script did not run to its end:\n%s", out)
		}
	})
}

func TestKitRunOnAChannelThatIsThereOnlyChangesItsTransports(t *testing.T) {
	eachShell(t, func(t *testing.T, shell string) {
		pin := serveInstaller(t, fakeInstaller)
		kit, err := renderKit(kitChannel(t), pin, kitOptions{skipRootCheck: true})
		if err != nil {
			t.Fatal(err)
		}
		state, out, err := runKit(t, shell, kit, "of-kit")
		if err != nil {
			t.Fatalf("kit failed: %v\n%s", err, out)
		}
		if got := strings.Fields(readFile(t, filepath.Join(state, "calls"))); strings.Join(got, ",") != "list,set-transports" {
			t.Fatalf("installer calls = %v, want list, set-transports", got)
		}
		conf := readFile(t, filepath.Join(state, "set.conf"))
		if strings.Contains(conf, "key=") || strings.Contains(conf, "port=") {
			t.Errorf("set-transports must keep the channel's key and port, not be given new ones:\n%s", shorten(conf))
		}
		if !containsLine(conf, "channel=of-kit") || !strings.Contains(conf, "script=mx,") {
			t.Errorf("set-transports config:\n%s", shorten(conf))
		}
		// a different channel on the machine is no reason to touch it
		state2, _, err := runKit(t, shell, kit, "of-other")
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Fields(readFile(t, filepath.Join(state2, "calls"))); strings.Join(got, ",") != "list,plan,apply" {
			t.Errorf("installer calls with another channel there = %v", got)
		}
	})
}

func TestKitStopsBeforeRunningAnInstallerThatIsNotTheChecked(t *testing.T) {
	eachShell(t, func(t *testing.T, shell string) {
		pin := serveInstaller(t, fakeInstaller)
		pin.SHA256 = strings.Repeat("0", 64) // the pin says another file
		kit, err := renderKit(kitChannel(t), pin, kitOptions{skipRootCheck: true})
		if err != nil {
			t.Fatal(err)
		}
		state, out, err := runKit(t, shell, kit, "")
		if err == nil || !strings.Contains(out, "отличается от проверенного") {
			t.Fatalf("a tampered installer ran: err=%v\n%s", err, out)
		}
		if _, statErr := os.Stat(filepath.Join(state, "calls")); statErr == nil {
			t.Fatal("the installer was run although its hash did not match")
		}
	})
}

func TestKitReportsAnInstallerThatRefuses(t *testing.T) {
	eachShell(t, func(t *testing.T, shell string) {
		refusing := strings.Replace(fakeInstaller, `echo '{"ok":true,"channel":"of-kit","port":43210}'`, `echo '{"ok":false,"step":"apply","error":"порт 43210 занят"}'`, 1)
		pin := serveInstaller(t, refusing)
		kit, err := renderKit(kitChannel(t), pin, kitOptions{skipRootCheck: true})
		if err != nil {
			t.Fatal(err)
		}
		_, out, err := runKit(t, shell, kit, "")
		if err == nil || !strings.Contains(out, "порт 43210 занят") {
			t.Fatalf("a refusal was not shown: err=%v\n%s", err, out)
		}
	})
}

func TestKitRefusesWhatTheInstallerWould(t *testing.T) {
	good := kitChannel(t)
	cases := map[string]func(c *Channel){
		"bad channel": func(c *Channel) { c.ID = "Of_Kit" },
		"short key":   func(c *Channel) { c.Key = "abc" },
		"no package":  func(c *Channel) { c.Transports[1].Package = "" },
		"bad script":  func(c *Channel) { c.Transports[1].Pubkey = "zz" },
		"bad link":    func(c *Channel) { c.Transports[0].URL = "http://cloud.mail.ru/x" },
		"bad mode":    func(c *Channel) { c.Mode = "l5" },
	}
	for name, mutate := range cases {
		c := good
		c.Transports = append([]ChannelTransport(nil), good.Transports...)
		mutate(&c)
		if _, err := RenderKit(c, ""); err == nil {
			t.Errorf("%s: a kit was made", name)
		}
	}
	// a plain-http installer is refused outside tests
	if _, err := renderKit(good, Script{URL: "http://example.com/x.sh", SHA256: strings.Repeat("a", 64)}, kitOptions{}); err == nil {
		t.Error("a plain-http installer was accepted")
	}
	if _, err := renderKit(good, Script{URL: "https://example.com/x'.sh", SHA256: strings.Repeat("a", 64)}, kitOptions{}); err == nil {
		t.Error("a quote in the installer's address was accepted")
	}
}

func TestKitSecretsStayOutOfTheScriptsNonSecretParts(t *testing.T) {
	// The config's non-script lines are the installer's own (Channel.config) minus the port: no drift from the wizard's.
	ch := kitChannel(t)
	ch.Transports = ch.Transports[:1]
	kit, err := renderKit(ch, Pinned(), kitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want, err := ch.config()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(kit.Script, "<<'OPENFLUX_NODE_CONF'\n"+want+"OPENFLUX_NODE_CONF\n") {
		t.Errorf("the kit's config differs from Channel.config:\nwant\n%s\nscript\n%s", want, kit.Script)
	}
}

func containsLine(text, line string) bool {
	for _, l := range strings.Split(text, "\n") {
		if l == line {
			return true
		}
	}
	return false
}

func shorten(s string) string {
	if len(s) > 600 {
		return s[:600] + "…"
	}
	return s
}
