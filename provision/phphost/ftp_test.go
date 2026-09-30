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
	srv := newFakeFTP(t, "u", "pw", "htdocs")
	srv.truncate = true
	_, err := Deploy(ctx(t), srv.target(), "", nil)
	if code(err) != CodeUpload {
		t.Fatalf("a half-stored file went unnoticed: %v", err)
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
