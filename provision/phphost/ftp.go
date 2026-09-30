package phphost

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jlaffaye/ftp"

	bundle "github.com/p1neappleXpress/OpenFlux/deploy/phpbox"
)

// FTP is how to reach the account. The folder the site is served from is
// Dir, or found when empty.
type FTP struct {
	Host     string `json:"host"`
	Port     int    `json:"port,omitempty"` // 21, or 990 for implicit TLS
	User     string `json:"user"`
	Password string `json:"password"`
	// TLS is "auto" (the default: explicit TLS, then explicit TLS without checking the
	// certificate, then plain - the security actually got is reported), "none",
	// "explicit" (AUTH TLS) or "implicit".
	TLS string `json:"tls,omitempty"`
	Dir string `json:"dir,omitempty"`
}

// Security levels of the FTP control connection that was got.
const (
	SecurityTLS           = "tls"            // TLS, certificate checked
	SecurityTLSUnverified = "tls_unverified" // TLS, but the host's certificate does not match its name (common on shared hosting)
	SecurityNone          = "none"           // plain FTP: the password crossed the network in the clear
)

func (t FTP) addr() string {
	port := t.Port
	if port == 0 {
		port = 21
		if t.TLS == "implicit" {
			port = 990
		}
	}
	return net.JoinHostPort(strings.TrimSpace(t.Host), strconv.Itoa(port))
}

func (t FTP) validate() error {
	if strings.TrimSpace(t.Host) == "" || t.User == "" || t.Password == "" {
		return fail(CodeBadParams, "host/user/password", "phphost: ftp host, user and password are required", nil)
	}
	return nil
}

type session struct {
	c        *ftp.ServerConn
	security string
}

func (s *session) close() { _ = s.c.Quit() }

var certProblem = regexp.MustCompile(`(?i)x509|certificate|tls: failed to verify`)

// dial opens and logs in, choosing the TLS as t.TLS says.
func dial(ctx context.Context, t FTP) (*session, error) {
	if err := t.validate(); err != nil {
		return nil, err
	}
	type attempt struct {
		name string
		opt  []ftp.DialOption
		sec  string
	}
	host := strings.TrimSpace(t.Host)
	verified := &tls.Config{ServerName: host}
	lax := &tls.Config{ServerName: host, InsecureSkipVerify: true}
	base := []ftp.DialOption{ftp.DialWithContext(ctx), ftp.DialWithTimeout(20 * time.Second)}
	var attempts []attempt
	switch t.TLS {
	case "none":
		attempts = []attempt{{"plain", base, SecurityNone}}
	case "implicit":
		attempts = []attempt{
			{"implicit", append(append([]ftp.DialOption{}, base...), ftp.DialWithTLS(verified)), SecurityTLS},
			{"implicit-lax", append(append([]ftp.DialOption{}, base...), ftp.DialWithTLS(lax)), SecurityTLSUnverified},
		}
	case "explicit":
		attempts = []attempt{
			{"explicit", append(append([]ftp.DialOption{}, base...), ftp.DialWithExplicitTLS(verified)), SecurityTLS},
			{"explicit-lax", append(append([]ftp.DialOption{}, base...), ftp.DialWithExplicitTLS(lax)), SecurityTLSUnverified},
		}
	default: // auto
		attempts = []attempt{
			{"explicit", append(append([]ftp.DialOption{}, base...), ftp.DialWithExplicitTLS(verified)), SecurityTLS},
			{"explicit-lax", append(append([]ftp.DialOption{}, base...), ftp.DialWithExplicitTLS(lax)), SecurityTLSUnverified},
			{"plain", base, SecurityNone},
		}
	}
	var lastErr error
	reached := false
	for _, a := range attempts {
		if ctx.Err() != nil {
			return nil, fail(CodeFTPConnect, t.addr(), "phphost: cancelled", ctx.Err())
		}
		c, err := ftp.Dial(t.addr(), a.opt...)
		if err != nil {
			lastErr = err
			if isNetworkDown(err) {
				break // nothing answers there: a weaker TLS will not help
			}
			continue
		}
		reached = true
		if err := c.Login(t.User, t.Password); err != nil {
			_ = c.Quit()
			if isLoginRefused(err) {
				return nil, fail(CodeFTPLogin, "", "phphost: the FTP server refused the login", nil)
			}
			lastErr = err
			continue
		}
		return &session{c: c, security: a.sec}, nil
	}
	if !reached {
		return nil, fail(CodeFTPConnect, t.addr(), "phphost: cannot reach the FTP server", lastErr)
	}
	if lastErr != nil && certProblem.MatchString(lastErr.Error()) {
		return nil, fail(CodeFTPTLS, "", "phphost: the FTP server's TLS could not be set up", lastErr)
	}
	return nil, fail(CodeFTPConnect, t.addr(), "phphost: cannot talk to the FTP server", lastErr)
}

func isNetworkDown(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	var op *net.OpError
	return errors.As(err, &op)
}

func isLoginRefused(err error) bool {
	var te *textproto.Error
	if errors.As(err, &te) {
		return te.Code == 530 || te.Code == 430 || te.Code == 331
	}
	return false
}

// webRootNames are where hosts serve the site from, most common first.
var webRootNames = []string{"htdocs", "public_html", "www", "httpdocs", "web", "html", "public", "wwwroot", "www.root"}

// Probe is what ProbeHost learned about the account.
type Probe struct {
	Security   string   `json:"security"`   // SecurityTLS | SecurityTLSUnverified | SecurityNone
	Dir        string   `json:"dir"`        // the folder the site is served from ("" = the login folder itself)
	Candidates []string `json:"candidates"` // folders found when it was not clear which
	Writable   bool     `json:"writable"`
	HasNode    bool     `json:"has_node"`        // a node is installed there already
	Token      string   `json:"token,omitempty"` // its token, kept on a reinstall so its addresses stay valid
}

var tokenRe = regexp.MustCompile(`define\('PHPBOX_TOKEN',\s*'([0-9A-Za-z_-]+)'\)`)

// ProbeHost logs in, finds the web root and checks it can be written to.
func ProbeHost(ctx context.Context, t FTP) (*Probe, error) {
	s, err := dial(ctx, t)
	if err != nil {
		return nil, err
	}
	defer s.close()
	return probe(s, t)
}

func probe(s *session, t FTP) (*Probe, error) {
	p := &Probe{Security: s.security}
	dir := strings.Trim(strings.TrimSpace(t.Dir), "/")
	if t.Dir != "" {
		if dir != "" {
			if _, err := s.c.List(dir); err != nil {
				return nil, fail(CodeDirMissing, dir, "phphost: no such folder", err)
			}
		}
		p.Dir = dir
	} else {
		entries, err := s.c.List("")
		if err != nil {
			return nil, fail(CodeFTPConnect, t.addr(), "phphost: cannot list the login folder", err)
		}
		var dirs []string
		have := map[string]bool{}
		hasIndex := false
		for _, e := range entries {
			if e.Name == "." || e.Name == ".." {
				continue
			}
			have[e.Name] = true
			if e.Type == ftp.EntryTypeFolder {
				dirs = append(dirs, e.Name)
			} else if n := strings.ToLower(e.Name); strings.HasPrefix(n, "index.") || n == ".htaccess" || n == "wp-config.php" {
				hasIndex = true
			}
		}
		sort.Strings(dirs)
		p.Candidates = dirs
		found := false
		for _, n := range webRootNames {
			if have[n] {
				p.Dir, found = n, true
				break
			}
		}
		if !found && hasIndex {
			p.Dir, found = "", true // the login folder is served
		}
		if !found {
			// Other hosts keep the site a level or two down: domains/example.com/public_html,
			// www/example.com, sites/example.com/htdocs. One such folder is taken as the answer;
			// several are offered, with their paths, for the user to choose.
			deeper := findDeeper(s, dirs)
			p.Candidates = deeper
			if len(deeper) == 1 {
				p.Dir, found = deeper[0], true
			}
		}
		if !found {
			cands := p.Candidates
			if len(cands) == 0 {
				cands = dirs
			}
			p.Candidates = cands
			return p, fail(CodeNoWebRoot, strings.Join(cands, ","), "phphost: cannot tell which folder the site is served from", nil)
		}
	}
	// An earlier install: keep its token.
	if r, err := s.c.Retr(join(p.Dir, "config.php")); err == nil {
		b, _ := io.ReadAll(io.LimitReader(r, 4096))
		r.Close()
		if m := tokenRe.FindSubmatch(b); m != nil {
			p.HasNode, p.Token = true, string(m[1])
		}
	}
	// Can we write there?
	probeFile := join(p.Dir, ".phpbox-write-test")
	if err := s.c.Stor(probeFile, bytes.NewReader([]byte("x"))); err != nil {
		return p, fail(CodeNotWritable, p.Dir, "phphost: the web folder is not writable", err)
	}
	_ = s.c.Delete(probeFile)
	p.Writable = true
	return p, nil
}

// findDeeper looks one and two levels below the login folder for folders that
// look like a site's web root: top/x where x is a usual web root name, and
// top/x/root for a top folder that holds several sites (domains, sites, www, ...).
func findDeeper(s *session, top []string) []string {
	isRoot := map[string]bool{}
	for _, n := range webRootNames {
		isRoot[n] = true
	}
	var found []string
	limit := 0
	for _, d := range top {
		if limit++; limit > 16 || strings.HasPrefix(d, ".") {
			continue
		}
		entries, err := s.c.List(d)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.Type != ftp.EntryTypeFolder || e.Name == "." || e.Name == ".." {
				continue
			}
			if isRoot[e.Name] {
				found = append(found, path.Join(d, e.Name))
				continue
			}
			// a site folder (example.com) holding its own web root
			sub, err := s.c.List(path.Join(d, e.Name))
			if err != nil {
				continue
			}
			for _, x := range sub {
				if x.Type == ftp.EntryTypeFolder && isRoot[x.Name] {
					found = append(found, path.Join(d, e.Name, x.Name))
				}
			}
		}
	}
	sort.Strings(found)
	return found
}

func join(dir, name string) string {
	if dir == "" {
		return name
	}
	return path.Join(dir, name)
}

// Progress is reported while Deploy uploads.
type Progress struct {
	Phase      string `json:"phase"` // "connect" | "probe" | "upload" | "done"
	File       string `json:"file,omitempty"`
	N          int    `json:"n"`          // files finished
	Of         int    `json:"of"`         // files in all
	BytesDone  int64  `json:"bytes_done"` // over all files
	BytesTotal int64  `json:"bytes_total"`
}

// Installed is what Deploy answers.
type Installed struct {
	Dir      string `json:"dir"`
	Token    string `json:"token"`
	Files    int    `json:"files"`
	Bytes    int64  `json:"bytes"`
	Security string `json:"security"`
	Reused   bool   `json:"token_reused"` // the node was installed before; its token was kept
}

type countingReader struct {
	r    io.Reader
	done *int64
	tick func()
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	*c.done += int64(n)
	if n > 0 && c.tick != nil {
		c.tick()
	}
	return n, err
}

// Deploy uploads the bundle into the account's web folder. token "" keeps an
// installed node's token, or makes a new one. progress may be nil.
func Deploy(ctx context.Context, t FTP, token string, progress func(Progress)) (*Installed, error) {
	if progress == nil {
		progress = func(Progress) {}
	}
	progress(Progress{Phase: "connect"})
	s, err := dial(ctx, t)
	if err != nil {
		return nil, err
	}
	defer s.close()
	progress(Progress{Phase: "probe"})
	p, err := probe(s, t)
	if err != nil {
		return nil, err
	}
	reused := false
	switch {
	case token != "":
	case p.Token != "":
		token, reused = p.Token, true
	default:
		if token, err = NewToken(); err != nil {
			return nil, err
		}
	}

	files := bundle.Files()
	files = append(files,
		bundle.File{Path: "lib/.htaccess", Data: []byte("Require all denied\n")},
		bundle.File{Path: "config.php", Data: []byte(configPHP(token))}, // last: a half-uploaded node has no token yet
	)
	var total int64
	for _, f := range files {
		total += int64(len(f.Data))
	}
	for _, d := range []string{"lib", "assets"} {
		_ = s.c.MakeDir(join(p.Dir, d)) // exists already on a reinstall
	}
	var done int64
	for i, f := range files {
		if ctx.Err() != nil {
			return nil, fail(CodeUpload, f.Path, "phphost: cancelled", ctx.Err())
		}
		start := done
		cr := &countingReader{r: bytes.NewReader(f.Data), done: &done}
		cr.tick = func() {
			progress(Progress{Phase: "upload", File: f.Path, N: i, Of: len(files), BytesDone: done, BytesTotal: total})
		}
		if err := s.c.Stor(join(p.Dir, f.Path), cr); err != nil {
			return nil, fail(CodeUpload, f.Path, "phphost: upload failed", err)
		}
		done = start + int64(len(f.Data))
		progress(Progress{Phase: "upload", File: f.Path, N: i + 1, Of: len(files), BytesDone: done, BytesTotal: total})
		// A truncated upload is the usual silent failure on shared hosting: check the size where the server can say.
		if sz, err := s.c.FileSize(join(p.Dir, f.Path)); err == nil && sz != int64(len(f.Data)) {
			return nil, fail(CodeUpload, f.Path, fmt.Sprintf("phphost: %s arrived as %d bytes, sent %d", f.Path, sz, len(f.Data)), nil)
		}
	}
	progress(Progress{Phase: "done", N: len(files), Of: len(files), BytesDone: total, BytesTotal: total})
	return &Installed{Dir: p.Dir, Token: token, Files: len(files), Bytes: total, Security: s.security, Reused: reused}, nil
}

// configPHP is config.php: the token as a constant (putenv is disabled on most
// free hosts, so the exits read the constant too).
func configPHP(token string) string {
	return "<?php\n// written by OpenFlux: the token that guards this node's pages\n" +
		"if (!defined('PHPBOX_CONFIG_LOADED')) {\n    define('PHPBOX_CONFIG_LOADED', 1);\n" +
		"    if (!defined('PHPBOX_TOKEN')) define('PHPBOX_TOKEN', '" + token + "');\n}\n"
}

// Remove deletes the files Deploy uploaded (nothing else in the folder).
func Remove(ctx context.Context, t FTP) error {
	s, err := dial(ctx, t)
	if err != nil {
		return err
	}
	defer s.close()
	p, err := probe(s, t)
	if err != nil {
		return err
	}
	files := bundle.Files()
	files = append(files, bundle.File{Path: "lib/.htaccess"}, bundle.File{Path: "config.php"})
	for _, f := range files {
		_ = s.c.Delete(join(p.Dir, f.Path))
	}
	_ = s.c.RemoveDir(join(p.Dir, "lib"))
	_ = s.c.RemoveDir(join(p.Dir, "assets"))
	return nil
}
