package phphost

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// challenge builds the browser-check page of an iFastNet host for a cookie value.
func challenge(value string) (page string) {
	key := []byte("0123456789abcdef")
	iv := []byte("fedcba9876543210")
	pt, _ := hex.DecodeString(value)
	ct := make([]byte, len(pt))
	blk, _ := aes.NewCipher(key)
	cipher.NewCBCEncrypter(blk, iv).CryptBlocks(ct, pt)
	return fmt.Sprintf(`<html><body><script type="text/javascript" src="/aes.js"></script><script>function toNumbers(d){}
var a=toNumbers("%x"),b=toNumbers("%x"),c=toNumbers("%x");document.cookie="__test="+toHex(slowAES.decrypt(c,2,a,b))+"; path=/";location.href="http://x/?i=1";</script></body></html>`, key, iv, ct)
}

const secretCookie = "00112233445566778899aabbccddeeff"

// fakeHost serves a node's pages behind the browser check: no cookie, the
// challenge; with the cookie, what handler says.
func fakeHost(t *testing.T, handler func(w http.ResponseWriter, q url.Values)) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("__test"); err != nil || c.Value != secretCookie {
			fmt.Fprint(w, challenge(secretCookie))
			return
		}
		handler(w, r.URL.Query())
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSolveAntiBotMatchesWhatABrowserComputes(t *testing.T) {
	name, val, ok := solveAntiBot([]byte(challenge(secretCookie)))
	if !ok || name != "__test" || val != secretCookie {
		t.Fatalf("got %q %q %v, want __test %s", name, val, ok, secretCookie)
	}
	for _, page := range []string{"<html>hello</html>", `{"ok":true}`, "slowAES but no numbers"} {
		if _, _, ok := solveAntiBot([]byte(page)); ok {
			t.Errorf("%q taken for the browser check", page)
		}
	}
}

func TestCheckPassesTheBrowserCheckAndReadsThePing(t *testing.T) {
	var hits atomic.Int32
	h := fakeHost(t, func(w http.ResponseWriter, q url.Values) {
		hits.Add(1)
		if q.Get("k") != "tok" {
			http.Error(w, "no", 404)
			return
		}
		fmt.Fprint(w, `{"phpbox":"0.4","carrier":"mailru","php":"8.4.1","missing":[],"state_dir":true,"parser":true}`)
	})
	s := &Site{URL: h.URL, Token: "tok", Carrier: "mailru"}
	st, err := s.Check(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	if st.Version != "0.4" || st.PHP != "8.4.1" || !st.Parser {
		t.Errorf("status %+v", st)
	}
	if hits.Load() != 1 {
		t.Errorf("the page was asked for %d times after the check", hits.Load())
	}

	bad := &Site{URL: h.URL, Token: "wrong", Carrier: "mailru"}
	if _, err := bad.Check(ctx(t)); code(err) != CodeTokenRefused {
		t.Errorf("wrong token: %v (%s)", err, code(err))
	}
}

func TestCheckErrorsAreCodes(t *testing.T) {
	serve := func(body string, status int) *Site {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status); fmt.Fprint(w, body) }))
		t.Cleanup(srv.Close)
		return &Site{URL: srv.URL, Token: "t", Carrier: "cupsonline"}
	}
	if _, err := serve("<html>Welcome to my site</html>", 200).Check(ctx(t)); code(err) != CodeNotPhpbox {
		t.Errorf("an ordinary page: %v", err)
	}
	if _, err := serve("<html>Just a moment... captcha</html>", 200).Check(ctx(t)); code(err) != CodeAntiBot {
		t.Errorf("a check we cannot pass: %v", err)
	}
	if _, err := serve(`{"phpbox":"0.4","missing":["usleep","openssl"]}`, 200).Check(ctx(t)); code(err) != CodePHPMissing {
		t.Errorf("php lacking functions: %v", err)
	} else if e := err.(*Error); e.Param != "usleep,openssl" {
		t.Errorf("param %q", e.Param)
	}
	if _, err := (&Site{URL: "http://127.0.0.1:1", Token: "t"}).Check(ctx(t)); code(err) != CodeSiteUnreachable {
		t.Errorf("nothing there: %v", err)
	}
	if _, err := (&Site{URL: "", Token: "t"}).Check(ctx(t)); code(err) != CodeBadParams {
		t.Errorf("no url: %v", err)
	}
}

// A node that is told to run reports running; one that already runs is left alone.
func TestStartRunsTheNodeOnceAndStopEndsIt(t *testing.T) {
	var running atomic.Bool
	var runs atomic.Int32
	var gotChain atomic.Bool
	h := fakeHost(t, func(w http.ResponseWriter, q url.Values) {
		switch q.Get("a") {
		case "run":
			runs.Add(1)
			gotChain.Store(q.Get("chain") == "1")
			running.Store(true)
			time.Sleep(200 * time.Millisecond) // the request IS the node: it does not answer quickly
		case "status":
			fmt.Fprintf(w, `{"running":%v,"draining":0,"chain":true,"state":{"gen":1,"phase":"serving"}}`, running.Load())
		case "stop":
			running.Store(false)
			fmt.Fprint(w, `{"ok":true}`)
		}
	})
	s := &Site{URL: h.URL, Token: "t", Carrier: "cupsonline"}
	ns, err := s.Start(ctx(t), "https://interview.cups.online/live-coding/?room=r", StartOptions{Chain: true}, 10*time.Second)
	if err != nil || !ns.Running || ns.State.Gen != 1 {
		t.Fatalf("Start: %+v %v", ns, err)
	}
	if !gotChain.Load() {
		t.Error("chain mode was not asked for")
	}
	// Already running: no second run request.
	if _, err := s.Start(ctx(t), "https://interview.cups.online/live-coding/?room=r", StartOptions{Chain: true}, 5*time.Second); err != nil || runs.Load() != 1 {
		t.Errorf("a second Start ran the node again (%d runs, err %v)", runs.Load(), err)
	}
	if err := s.Stop(ctx(t), "https://interview.cups.online/live-coding/?room=r"); err != nil {
		t.Fatal(err)
	}
	if now, _ := s.Node(ctx(t), "x"); now == nil || now.Running {
		t.Errorf("after Stop: %+v", now)
	}
}

func TestStartGivesUpWhenTheNodeNeverRuns(t *testing.T) {
	h := fakeHost(t, func(w http.ResponseWriter, q url.Values) {
		if q.Get("a") == "status" {
			fmt.Fprint(w, `{"running":false,"draining":0,"chain":false,"state":{}}`)
		}
	})
	s := &Site{URL: h.URL, Token: "t", Carrier: "mailru"}
	if _, err := s.Start(ctx(t), "doc", StartOptions{}, 2*time.Second); code(err) != CodeNodeNotStarted {
		t.Errorf("a node that never ran: %v", err)
	}
}

// The whole path on the real thing: deploy over FTP, serve what landed with PHP, ask it.
func TestDeployedFilesRunUnderPHP(t *testing.T) {
	php, err := exec.LookPath("php")
	if err != nil {
		t.Skip("no php on this machine")
	}
	srv := newFakeFTP(t, "u", "pw", "htdocs")
	in, err := Deploy(ctx(t), srv.target(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "htdocs")
	for p, b := range srv.files {
		rel := strings.TrimPrefix(p, "/htdocs/")
		if rel == p {
			continue
		}
		dst := filepath.Join(root, rel)
		_ = os.MkdirAll(filepath.Dir(dst), 0o755)
		_ = os.WriteFile(dst, b, 0o644)
	}
	port := freePort(t)
	cmd := exec.Command(php, "-S", fmt.Sprintf("127.0.0.1:%d", port), "-t", root)
	cmd.Env = append(os.Environ(), "TMPDIR="+t.TempDir(), "PHP_CLI_SERVER_WORKERS=4")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	site := &Site{URL: fmt.Sprintf("http://127.0.0.1:%d", port), Token: in.Token, Carrier: "mailru"}
	var st *Status
	for i := 0; i < 30; i++ {
		if st, err = site.Check(ctx(t)); err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("the deployed node does not answer: %v", err)
	}
	if st.Carrier != "mailru" || !st.StateDir || !st.Parser {
		t.Errorf("ping: %+v", st)
	}
	if _, err := (&Site{URL: site.URL, Token: "not-the-token", Carrier: "mailru"}).Check(ctx(t)); code(err) != CodeTokenRefused {
		t.Errorf("a wrong token was accepted by the deployed node: %v", err)
	}
	if ns, err := site.Node(ctx(t), "some-doc"); err != nil || ns.Running {
		t.Errorf("a fresh node reports %+v %v", ns, err)
	}
}

func freePort(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}
