package phphost

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	bundle "github.com/p1neappleXpress/OpenFlux/deploy/phpbox"
)

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return c
}

func code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func TestProbeFindsTheWebRoot(t *testing.T) {
	srv := newFakeFTP(t, "if0_1", "pw", "htdocs", "logs")
	p, err := ProbeHost(ctx(t), srv.target())
	if err != nil {
		t.Fatal(err)
	}
	if p.Dir != "htdocs" || !p.Writable || p.HasNode || p.Security != SecurityNone {
		t.Errorf("probe = %+v", p)
	}
	if strings.Join(p.Candidates, ",") != "htdocs,logs" {
		t.Errorf("candidates %v", p.Candidates)
	}
	// Other hosts name it differently.
	srv2 := newFakeFTP(t, "u", "pw", "public_html", "mail")
	if p2, err := ProbeHost(ctx(t), srv2.target()); err != nil || p2.Dir != "public_html" {
		t.Errorf("public_html: %+v %v", p2, err)
	}
}

func TestProbeErrorsAreCodes(t *testing.T) {
	srv := newFakeFTP(t, "u", "pw", "htdocs")
	bad := srv.target()
	bad.Password = "wrong"
	if _, err := ProbeHost(ctx(t), bad); code(err) != CodeFTPLogin {
		t.Errorf("wrong password: %v (%s)", err, code(err))
	}
	if _, err := ProbeHost(ctx(t), FTP{Host: "127.0.0.1", Port: 1, User: "u", Password: "p", TLS: "none"}); code(err) != CodeFTPConnect {
		t.Errorf("nothing listening: %v", err)
	}
	if _, err := ProbeHost(ctx(t), FTP{Host: "", User: "u", Password: "p"}); code(err) != CodeBadParams {
		t.Errorf("no host: %v", err)
	}

	unclear := newFakeFTP(t, "u", "pw", "alpha", "beta")
	_, err := ProbeHost(ctx(t), unclear.target())
	var e *Error
	if !errors.As(err, &e) || e.Code != CodeNoWebRoot || e.Param != "alpha,beta" {
		t.Errorf("unclear root: %v", err)
	}
	// ... which the user settles by naming it.
	named := unclear.target()
	named.Dir = "beta"
	if p, err := ProbeHost(ctx(t), named); err != nil || p.Dir != "beta" {
		t.Errorf("named dir: %+v %v", p, err)
	}
	named.Dir = "nope"
	if _, err := ProbeHost(ctx(t), named); code(err) != CodeDirMissing {
		t.Errorf("missing dir: %v", err)
	}

	ro := newFakeFTP(t, "u", "pw", "htdocs")
	ro.readOnly = true
	if _, err := ProbeHost(ctx(t), ro.target()); code(err) != CodeNotWritable {
		t.Errorf("read only: %v", err)
	}
}

// "auto" TLS against a server that has none ends on plain FTP, and says so.
func TestAutoTLSFallsBackAndReportsIt(t *testing.T) {
	srv := newFakeFTP(t, "u", "pw", "htdocs")
	tg := srv.target()
	tg.TLS = "auto"
	p, err := ProbeHost(ctx(t), tg)
	if err != nil {
		t.Fatal(err)
	}
	if p.Security != SecurityNone {
		t.Errorf("security = %q, want %q", p.Security, SecurityNone)
	}
}

func TestDeployUploadsTheBundleAndKeepsTheTokenOnReinstall(t *testing.T) {
	srv := newFakeFTP(t, "u", "pw", "htdocs")
	var steps []Progress
	in, err := Deploy(ctx(t), srv.target(), "", func(p Progress) { steps = append(steps, p) })
	if err != nil {
		t.Fatal(err)
	}
	if in.Dir != "htdocs" || len(in.Token) != 24 || in.Reused {
		t.Errorf("installed = %+v", in)
	}
	for _, f := range bundle.Files() {
		got, ok := srv.files["/htdocs/"+f.Path]
		if !ok || string(got) != string(f.Data) {
			t.Errorf("%s was not uploaded intact (%d bytes there)", f.Path, len(got))
		}
	}
	cfg := string(srv.files["/htdocs/config.php"])
	if !strings.Contains(cfg, "PHPBOX_TOKEN', '"+in.Token+"'") {
		t.Errorf("config.php does not carry the token:\n%s", cfg)
	}
	if string(srv.files["/htdocs/lib/.htaccess"]) != "Require all denied\n" {
		t.Error("lib/ is not shut off from the web")
	}
	if _, leaked := srv.files["/htdocs/.phpbox-write-test"]; leaked {
		t.Error("the write probe was left on the host")
	}

	// Progress: starts with connect, ends done, bytes never go backwards and reach the total.
	if len(steps) < 4 || steps[0].Phase != "connect" || steps[len(steps)-1].Phase != "done" {
		t.Errorf("progress phases: first %+v last %+v", steps[0], steps[len(steps)-1])
	}
	var last int64
	for _, s := range steps {
		if s.BytesDone < last {
			t.Fatalf("progress went backwards: %d after %d", s.BytesDone, last)
		}
		last = s.BytesDone
	}
	if end := steps[len(steps)-1]; end.BytesDone != end.BytesTotal || end.BytesTotal != in.Bytes {
		t.Errorf("progress ended at %d of %d (installed %d)", end.BytesDone, end.BytesTotal, in.Bytes)
	}

	// Again: the node is there, its token is kept, so its addresses stay valid.
	again, err := Deploy(ctx(t), srv.target(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if again.Token != in.Token || !again.Reused {
		t.Errorf("reinstall changed the token: %+v vs %+v", again, in)
	}
	// ... unless one is asked for.
	named, _ := Deploy(ctx(t), srv.target(), "0123456789abcdef01234567", nil)
	if named.Token != "0123456789abcdef01234567" {
		t.Errorf("token asked for: %+v", named)
	}
}

func TestDeployCatchesATruncatedUpload(t *testing.T) {
	quickRetries(t)
	srv := newFakeFTP(t, "u", "pw", "htdocs")
	srv.truncate = true
	_, err := Deploy(ctx(t), srv.target(), "", nil)
	if code(err) != CodeUpload {
		t.Fatalf("a half-stored file went unnoticed: %v", err)
	}
}

func quickRetries(t *testing.T) {
	old := retryPause
	retryPause = time.Millisecond
	t.Cleanup(func() { retryPause = old })
}

// InfinityFree's Pure-FTPd cut the 2 MB parser short ("451 Transfer aborted") and the whole install failed on it.
func TestDeploySendsADroppedFileAgain(t *testing.T) {
	quickRetries(t)
	srv := newFakeFTP(t, "u", "pw", "htdocs")
	srv.abort["/htdocs/lib/mux.php"] = 2
	var retries []Progress
	in, err := Deploy(ctx(t), srv.target(), "", func(p Progress) {
		if p.Phase == "retry" {
			retries = append(retries, p)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range bundle.Files() {
		if got := srv.files["/htdocs/"+f.Path]; string(got) != string(f.Data) {
			t.Errorf("%s was not uploaded intact (%d bytes there)", f.Path, len(got))
		}
	}
	if len(retries) != 2 || retries[0].File != "lib/mux.php" || !strings.Contains(retries[0].Note, "Transfer aborted") {
		t.Errorf("retries = %+v", retries)
	}
	if srv.stors["/htdocs/lib/mux.php"] != 3 || len(in.Skipped) != 0 {
		t.Errorf("mux.php sent %d times, skipped %v", srv.stors["/htdocs/lib/mux.php"], in.Skipped)
	}
}

func TestDeployGivesUpOnAFileTheHostKeepsRefusing(t *testing.T) {
	quickRetries(t)
	srv := newFakeFTP(t, "u", "pw", "htdocs")
	srv.abort["/htdocs/lib/mux.php"] = 100
	_, err := Deploy(ctx(t), srv.target(), "", nil)
	var e *Error
	if !errors.As(err, &e) || e.Code != CodeUpload || e.Param != "lib/mux.php" {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "451") {
		t.Errorf("the host's own words are lost: %v", err)
	}
	if n := srv.stors["/htdocs/lib/mux.php"]; n != uploadTries {
		t.Errorf("mux.php sent %d times, want %d", n, uploadTries)
	}
}

// The parser only serves the page in a browser: a host that will not take it still gets a working node.
func TestDeployGoesOnWithoutTheOptionalParser(t *testing.T) {
	quickRetries(t)
	srv := newFakeFTP(t, "u", "pw", "htdocs")
	srv.abort["/htdocs/assets/share.wasm.gz"] = 100
	var skipped []Progress
	in, err := Deploy(ctx(t), srv.target(), "", func(p Progress) {
		if p.Phase == "skipped" {
			skipped = append(skipped, p)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Skipped) != 1 || in.Skipped[0] != "assets/share.wasm.gz" || len(skipped) != 1 {
		t.Fatalf("skipped = %v / %+v", in.Skipped, skipped)
	}
	for _, f := range bundle.Files() {
		if f.Path == "assets/share.wasm.gz" {
			continue
		}
		if got := srv.files["/htdocs/"+f.Path]; string(got) != string(f.Data) {
			t.Errorf("%s was not uploaded intact", f.Path)
		}
	}
	if !strings.Contains(string(srv.files["/htdocs/config.php"]), in.Token) {
		t.Error("config.php missing: the node would not run")
	}
}

func TestOnlyAutoLeavesTLSForPlainFTP(t *testing.T) {
	cases := []struct {
		asked, got string
		try        int
		want       string
	}{
		{"auto", SecurityTLS, 1, "auto"},           // one drop: TLS again
		{"auto", SecurityTLSUnverified, 2, "none"}, // two over TLS: plain, as curl
		{"", SecurityTLS, 2, "none"},
		{"auto", SecurityNone, 1, "none"},
		{"explicit", SecurityTLS, 3, "explicit"}, // the user asked for TLS: never weaker
		{"implicit", SecurityTLS, 3, "implicit"},
		{"none", SecurityNone, 1, "none"},
	}
	for _, c := range cases {
		if got := redialMode(c.asked, c.got, c.try); got != c.want {
			t.Errorf("redialMode(%q, %q, %d) = %q, want %q", c.asked, c.got, c.try, got, c.want)
		}
	}
}

func TestRemoveDeletesOnlyWhatWasUploaded(t *testing.T) {
	srv := newFakeFTP(t, "u", "pw", "htdocs")
	srv.files["/htdocs/mysite.html"] = []byte("<p>mine</p>")
	if _, err := Deploy(ctx(t), srv.target(), "", nil); err != nil {
		t.Fatal(err)
	}
	if err := Remove(ctx(t), srv.target()); err != nil {
		t.Fatal(err)
	}
	if _, ok := srv.files["/htdocs/config.php"]; ok {
		t.Error("config.php (the token) is still there")
	}
	if _, ok := srv.files["/htdocs/lib/mux.php"]; ok {
		t.Error("lib/mux.php is still there")
	}
	if string(srv.files["/htdocs/mysite.html"]) != "<p>mine</p>" {
		t.Error("the user's own file was touched")
	}
}

// Other hosts keep the site deeper than the login folder.
func TestProbeFindsAWebRootBelowTheLoginFolder(t *testing.T) {
	// domains/example.com/public_html: one site, taken as the answer.
	one := newFakeFTP(t, "u", "pw", "domains", "domains/example.com", "domains/example.com/public_html", "logs")
	p, err := ProbeHost(ctx(t), one.target())
	if err != nil {
		t.Fatal(err)
	}
	if p.Dir != "domains/example.com/public_html" || !p.Writable {
		t.Errorf("probe = %+v", p)
	}
	// It deploys there.
	if _, err := Deploy(ctx(t), one.target(), "", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := one.files["/domains/example.com/public_html/config.php"]; !ok {
		t.Error("the node was not uploaded into the site's folder")
	}

	// Two sites: the user chooses, and the choices carry the full paths.
	two := newFakeFTP(t, "u", "pw", "domains", "domains/a.com", "domains/a.com/public_html", "domains/b.org", "domains/b.org/public_html")
	_, err = ProbeHost(ctx(t), two.target())
	var e *Error
	if !errors.As(err, &e) || e.Code != CodeNoWebRoot || e.Param != "domains/a.com/public_html,domains/b.org/public_html" {
		t.Fatalf("two sites: %v", err)
	}
	chosen := two.target()
	chosen.Dir = "domains/b.org/public_html"
	if p, err := ProbeHost(ctx(t), chosen); err != nil || p.Dir != "domains/b.org/public_html" || !p.Writable {
		t.Errorf("chosen: %+v %v", p, err)
	}

	// www/<site>: a usual name one level down.
	www := newFakeFTP(t, "u", "pw", "sites", "sites/shop", "sites/shop/www")
	if p, err := ProbeHost(ctx(t), www.target()); err != nil || p.Dir != "sites/shop/www" {
		t.Errorf("sites/shop/www: %+v %v", p, err)
	}
}

func TestDeployTakesAChosenTokenOnlyOfASafeShape(t *testing.T) {
	srv := newFakeFTP(t, "u", "pw", "htdocs")
	for _, bad := range []string{"short", "has space in it", "quote'breaks-config", strings.Repeat("a", 65)} {
		if _, err := Deploy(ctx(t), srv.target(), bad, nil); code(err) != CodeBadParams {
			t.Errorf("token %q was taken: %v", bad, err)
		}
	}
	if len(srv.files) != 0 {
		t.Error("files were uploaded before the token was checked")
	}
	in, err := Deploy(ctx(t), srv.target(), "My-own_key-2026", nil)
	if err != nil || in.Token != "My-own_key-2026" {
		t.Fatalf("a good chosen token: %+v, %v", in, err)
	}
}
