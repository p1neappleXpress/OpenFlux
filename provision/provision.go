// Package provision installs an OpenFlux exit channel on a user's VDS over
// SSH. It is the phone side of deploy/node-install.sh: it connects, has the
// VDS download the script by a pinned commit, checks the script's SHA-256
// and runs it. Each channel is an independent node (its own document, key,
// port and systemd instance), so installing one never touches another.
//
// Secrets (SSH and sudo passwords, the private key, the channel key and the
// document URL) never go into command lines or error messages: the channel
// config travels as a 0600 temp file, the sudo password on stdin.
package provision

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

var randRead = rand.Read

// allowPlainScriptURL lets tests serve the script over http from loopback.
var allowPlainScriptURL = false

// Script is where the VDS downloads node-install.sh from. The commit and the
// hash are pinned together: changing the script needs a new commit, a new
// pin and a new app build, so a changed file on GitHub is never run.
type Script struct {
	URL    string
	SHA256 string
}

// Target is how to reach the VDS.
type Target struct {
	Host string
	Port int
	User string
	// Password or PrivateKey (PEM/OpenSSH), optionally with Passphrase.
	Password   string
	PrivateKey string
	Passphrase string
	// HostKey is the trusted SHA256 fingerprint ("SHA256:..."); empty on
	// the first connection, which then fails with a *HostKeyError so the
	// user can compare it and trust it.
	HostKey string
}

// HostKeyError reports a host key the user has not trusted yet (Mismatch
// false) or one that differs from the trusted key (Mismatch true).
type HostKeyError struct {
	Fingerprint string
	Mismatch    bool
}

func (e *HostKeyError) Error() string {
	if e.Mismatch {
		return "ключ сервера изменился: " + e.Fingerprint + ". Возможна подмена сервера"
	}
	return "новый сервер, отпечаток ключа " + e.Fingerprint
}

// ErrSudoPassword means sudo rejected the password.
var ErrSudoPassword = errors.New("sudo не принял пароль")

// Probe describes the VDS, as node-install.sh probe reports it.
type Probe struct {
	Arch       string   `json:"arch"`
	OS         string   `json:"os"`
	Systemd    bool     `json:"systemd"`
	Sudo       string   `json:"sudo"` // root, nopasswd, password, none
	Downloader string   `json:"downloader"`
	Firewall   string   `json:"firewall"`
	Core       string   `json:"core"`
	Channels   []string `json:"channels"`
}

// Plan is what apply will change, for the confirmation screen.
type Plan struct {
	Channel   string   `json:"channel"`
	Port      int      `json:"port"`
	Arch      string   `json:"arch"`
	Core      string   `json:"core"`
	Actions   []string `json:"actions"`
	Untouched []string `json:"untouched"`
}

// Channel is one channel's configuration.
type Channel struct {
	ID   string
	URL  string
	Key  string
	Port int
	// Cookies is the channel's Yandex sign-in for the node: the core's
	// cookie store JSON, base64-encoded (see CookieStore). Empty: none.
	Cookies string
}

// Conn is an SSH connection to the VDS with the script downloaded.
type Conn struct {
	client *ssh.Client
	script string // remote path of the verified script
	sudo   string
}

// Fingerprint formats a host key like OpenSSH does.
func Fingerprint(key ssh.PublicKey) string {
	return ssh.FingerprintSHA256(key)
}

// Dial connects and authenticates. With an empty or different t.HostKey it
// fails with a *HostKeyError before sending any credentials.
func Dial(ctx context.Context, t Target) (*Conn, error) {
	if t.Port == 0 {
		t.Port = 22
	}
	if t.Host == "" || t.User == "" {
		return nil, errors.New("укажите адрес сервера и логин")
	}
	var auths []ssh.AuthMethod
	if t.PrivateKey != "" {
		var signer ssh.Signer
		var err error
		if t.Passphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(t.PrivateKey), []byte(t.Passphrase))
		} else {
			signer, err = ssh.ParsePrivateKey([]byte(t.PrivateKey))
		}
		if err != nil {
			var missing *ssh.PassphraseMissingError
			if errors.As(err, &missing) {
				return nil, errors.New("ключ защищён паролем: укажите его")
			}
			return nil, errors.New("не удалось прочитать приватный ключ")
		}
		auths = append(auths, ssh.PublicKeys(signer))
	}
	if t.Password != "" {
		pw := t.Password
		auths = append(auths, ssh.Password(pw),
			ssh.KeyboardInteractive(func(_, _ string, questions []string, _ []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range answers {
					answers[i] = pw
				}
				return answers, nil
			}))
	}
	if len(auths) == 0 {
		return nil, errors.New("укажите пароль или приватный ключ")
	}
	config := &ssh.ClientConfig{
		User: t.User,
		Auth: auths,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			fp := Fingerprint(key)
			if t.HostKey == "" {
				return &HostKeyError{Fingerprint: fp}
			}
			if fp != t.HostKey {
				return &HostKeyError{Fingerprint: fp, Mismatch: true}
			}
			return nil
		},
		Timeout: 20 * time.Second,
	}
	addr := net.JoinHostPort(t.Host, strconv.Itoa(t.Port))
	dialer := net.Dialer{Timeout: 20 * time.Second}
	raw, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("нет соединения с %s: %w", addr, err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = raw.SetDeadline(deadline)
	}
	c, chans, reqs, err := ssh.NewClientConn(raw, addr, config)
	if err != nil {
		raw.Close()
		var hk *HostKeyError
		if errors.As(err, &hk) {
			return nil, hk
		}
		if strings.Contains(err.Error(), "unable to authenticate") {
			return nil, errors.New("сервер не принял логин, пароль или ключ")
		}
		return nil, fmt.Errorf("SSH: %v", err)
	}
	_ = raw.SetDeadline(time.Time{})
	return &Conn{client: ssh.NewClient(c, chans, reqs)}, nil
}

// Close removes the downloaded script and closes the connection.
func (c *Conn) Close() error {
	if c.script != "" {
		_, _, _ = c.run("rm -f "+shellQuote(c.script), nil)
	}
	return c.client.Close()
}

func (c *Conn) run(cmd string, stdin []byte) (stdout, stderr []byte, err error) {
	s, err := c.client.NewSession()
	if err != nil {
		return nil, nil, err
	}
	defer s.Close()
	var out, errb bytes.Buffer
	s.Stdout = &out
	s.Stderr = &errb
	if stdin != nil {
		s.Stdin = bytes.NewReader(stdin)
	}
	err = s.Run(cmd)
	return out.Bytes(), errb.Bytes(), err
}

var sha256Line = regexp.MustCompile(`(?m)^([0-9a-f]{64})\b`)

// FetchScript has the VDS download the installer and checks its SHA-256
// against the pinned one before anything runs it.
func (c *Conn) FetchScript(s Script) error {
	secure := strings.HasPrefix(s.URL, "https://") ||
		(allowPlainScriptURL && strings.HasPrefix(s.URL, "http://127.0.0.1:"))
	if !secure || strings.ContainsAny(s.URL, "'\"\\ \n") {
		return errors.New("неверный адрес скрипта")
	}
	cmd := `set -e; f=$(mktemp /tmp/openflux-node-install.XXXXXX); u='` + s.URL + `'
if command -v curl >/dev/null 2>&1; then curl -fsSL --retry 3 --connect-timeout 20 -o "$f" "$u"
elif command -v wget >/dev/null 2>&1; then wget -q -T 20 -t 3 -O "$f" "$u"
else echo "no-downloader" >&2; rm -f "$f"; exit 3; fi
chmod 0644 "$f"; echo "$f"
if command -v sha256sum >/dev/null 2>&1; then sha256sum "$f"; else openssl dgst -sha256 -r "$f"; fi`
	out, errb, err := c.run(cmd, nil)
	if err != nil {
		if strings.Contains(string(errb), "no-downloader") {
			return errors.New("на сервере нет curl или wget")
		}
		return fmt.Errorf("сервер не смог скачать скрипт установки с GitHub: %s", firstLine(errb))
	}
	lines := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)
	if len(lines) < 2 {
		return errors.New("не удалось проверить скрипт установки")
	}
	path := strings.TrimSpace(lines[0])
	m := sha256Line.FindStringSubmatch(lines[1])
	if m == nil || !strings.HasPrefix(path, "/tmp/openflux-node-install.") {
		return errors.New("не удалось проверить скрипт установки")
	}
	c.script = path
	if !strings.EqualFold(m[1], s.SHA256) {
		return errors.New("скрипт установки на GitHub отличается от проверенного, установка остановлена")
	}
	return nil
}

// Probe runs node-install.sh probe and remembers how to get root.
func (c *Conn) Probe() (*Probe, error) {
	var p Probe
	if err := c.script_("probe", nil, &p); err != nil {
		return nil, err
	}
	c.sudo = p.Sudo
	return &p, nil
}

// Plan asks what apply would change. port 0 lets the script pick one;
// withCookies adds the Yandex sign-in step.
func (c *Conn) Plan(channel string, port int, withCookies bool) (*Plan, error) {
	cfg := "channel=" + channel + "\n"
	if port != 0 {
		cfg += "port=" + strconv.Itoa(port) + "\n"
	}
	if withCookies {
		cfg += "cookies=yes\n"
	}
	var p Plan
	if err := c.script_("plan", []byte(cfg), &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// Apply installs and starts the channel. sudoPassword is used only when the
// account needs one.
func (c *Conn) Apply(ch Channel, sudoPassword string) error {
	cfg := fmt.Sprintf("channel=%s\nurl=%s\nkey=%s\nport=%d\n", ch.ID, ch.URL, ch.Key, ch.Port)
	if ch.Cookies != "" {
		cfg += "cookies=" + ch.Cookies + "\n"
	}
	return c.asRoot("apply", cfg, sudoPassword)
}

// SetCookies replaces an installed channel's Yandex sign-in (base64 cookie
// store JSON, see CookieStore) and restarts its node.
func (c *Conn) SetCookies(channel, cookies, sudoPassword string) error {
	return c.asRoot("set-cookies", "channel="+channel+"\ncookies="+cookies+"\n", sudoPassword)
}

// CookieStore turns a Cookie header ("a=1; b=2", as the WebView keeps it
// for the document) into what Channel.Cookies takes: the core's cookie
// store JSON, keyed by the document URL like the node looks it up, then
// base64. signedIn reports whether the header holds a Yandex login.
func CookieStore(documentURL, header string) (cookies string, signedIn bool, err error) {
	jar := map[string]string{}
	for _, part := range strings.Split(header, ";") {
		name, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || name == "" || strings.ContainsAny(name+value, "\r\n") {
			continue
		}
		jar[name] = value
	}
	if len(jar) == 0 {
		return "", false, errors.New("нет cookies Яндекса")
	}
	raw, err := json.Marshal(map[string]map[string]string{documentURL: jar})
	if err != nil {
		return "", false, err
	}
	_, signedIn = jar["Session_id"]
	return base64.StdEncoding.EncodeToString(raw), signedIn, nil
}

// Remove stops and deletes the channel; the last one also removes the
// binaries, unit and system user the script installed.
func (c *Conn) Remove(channel, sudoPassword string) error {
	return c.asRoot("remove", "channel="+channel+"\n", sudoPassword)
}

func (c *Conn) asRoot(command, cfg, sudoPassword string) error {
	if c.script == "" {
		return errors.New("скрипт установки не загружен")
	}
	// The config goes into a private temp file (the script deletes it right
	// after reading) so stdin is free for sudo's password.
	out, _, err := c.run(`umask 077; f=$(mktemp /tmp/openflux-node-conf.XXXXXX) && cat > "$f" && echo "$f"`, []byte(cfg))
	if err != nil {
		return errors.New("не удалось передать конфигурацию на сервер")
	}
	conf := strings.TrimSpace(string(out))
	if !strings.HasPrefix(conf, "/tmp/openflux-node-conf.") {
		return errors.New("не удалось передать конфигурацию на сервер")
	}
	script := "sh " + shellQuote(c.script) + " " + command + " " + shellQuote(conf)
	var stdin []byte
	switch c.sudo {
	case "root":
	case "nopasswd":
		script = "sudo -n " + script
	case "password":
		if sudoPassword == "" {
			_, _, _ = c.run("rm -f "+shellQuote(conf), nil)
			return errors.New("для sudo нужен пароль")
		}
		script = "sudo -S -p '' " + script
		stdin = []byte(sudoPassword + "\n")
	default:
		_, _, _ = c.run("rm -f "+shellQuote(conf), nil)
		return errors.New("у пользователя нет прав root и sudo")
	}
	stdout, stderr, runErr := c.run(script, stdin)
	var res struct {
		OK    bool   `json:"ok"`
		Step  string `json:"step"`
		Error string `json:"error"`
	}
	if jerr := lastJSON(stdout, &res); jerr != nil {
		// The script did not run (sudo refused, or it crashed). Its config
		// file may still be there.
		_, _, _ = c.run("rm -f "+shellQuote(conf), nil)
		if isSudoRefusal(stderr) {
			return ErrSudoPassword
		}
		if runErr != nil {
			return fmt.Errorf("%s не выполнен: %s", command, firstLine(stderr))
		}
		return jerr
	}
	if !res.OK {
		return fmt.Errorf("%s", res.Error)
	}
	return nil
}

// script_ runs a read-only command of the script and decodes its JSON.
func (c *Conn) script_(command string, stdin []byte, into interface{}) error {
	if c.script == "" {
		return errors.New("скрипт установки не загружен")
	}
	stdout, stderr, _ := c.run("sh "+shellQuote(c.script)+" "+command, stdin)
	var res struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := lastJSON(stdout, &res); err != nil {
		return fmt.Errorf("скрипт установки не ответил: %s", firstLine(stderr))
	}
	if !res.OK {
		return fmt.Errorf("%s", res.Error)
	}
	return lastJSON(stdout, into)
}

func lastJSON(out []byte, into interface{}) error {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if strings.HasPrefix(l, "{") {
			return json.Unmarshal([]byte(l), into)
		}
	}
	return errors.New("пустой ответ скрипта установки")
}

func isSudoRefusal(stderr []byte) bool {
	s := strings.ToLower(string(stderr))
	return strings.Contains(s, "incorrect password") || strings.Contains(s, "sorry, try again") ||
		strings.Contains(s, "password is required") || strings.Contains(s, "no password was provided") ||
		strings.Contains(s, "неверный пароль")
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

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// NewKey returns a fresh channel key: 32 random bytes, hex encoded, as
// --encryption-key-file and the app's Session profiles take it.
func NewKey() (string, error) {
	var b [32]byte
	if _, err := randRead(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// NewChannelID returns a short random channel name, "of-" and 6 base32
// characters, valid for node-install.sh.
func NewChannelID() (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz234567"
	var b [6]byte
	if _, err := randRead(b[:]); err != nil {
		return "", err
	}
	out := []byte("of-")
	for _, x := range b {
		out = append(out, alphabet[int(x)%len(alphabet)])
	}
	return string(out), nil
}

// ScriptHash returns the hex SHA-256 of a script body.
func ScriptHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
