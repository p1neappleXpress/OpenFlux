package script

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Update checking and applying for installed transports. The unit of an
// update is the signed .flux package; the author's update.json only says
// which package is current on which channel and where to fetch it. Nothing
// in update.json is trusted by itself: the package that comes back must
// verify under the key the user pinned when they first trusted the
// transport, and its own manifest must agree with the index (id, version,
// wire, api). So a hijacked index or mirror can at worst withhold updates.
//
// Everything here reports machine codes (Code*), never user text: the apps
// own the wording.

// Statuses of an UpdateReport.
const (
	UpdateUpToDate  = "up_to_date"
	UpdateAvailable = "available"
	UpdateBlocked   = "blocked" // exists, but not applicable as is (see Code)
	UpdateInstalled = "installed"
	UpdateError     = "error"
)

// Codes of an UpdateReport.
const (
	CodeNoSource        = "no_source"        // the transport declares no update URL
	CodeFetchFailed     = "fetch_failed"     // no source answered
	CodeInsecureURL     = "insecure_url"     // not https
	CodeBadIndex        = "bad_index"        // update.json does not parse or is unsupported
	CodeIDMismatch      = "id_mismatch"      // the index or package is for another transport
	CodeNoChannel       = "no_channel"       // the index has neither the channel nor stable
	CodeNeedsNewerApp   = "needs_newer_app"  // the update needs a newer host API than this core has
	CodeKeyChanged      = "key_changed"      // the index announces another author key: a new trust decision
	CodeWireBreak       = "wire_break"       // another wire generation: client and node must update together
	CodeBadHash         = "bad_hash"         // the download is not what the index promised
	CodeBadPackage      = "bad_package"      // not a readable .flux
	CodeBadSignature    = "bad_signature"    // not signed by the pinned key
	CodeVersionMismatch = "version_mismatch" // the package is not the version/wire/api the index announced
	CodeNotNewer        = "not_newer"        // the package is not newer than the installed one
	CodeInstallFailed   = "install_failed"   // could not write the package into place
	CodeNoPrevious      = "no_previous"      // rollback: there is no previous version
)

const (
	maxIndexBytes   = 256 << 10
	maxPackageBytes = 4 << 20
	indexFormat     = 1
)

// Installed is what the app knows about an installed transport.
type Installed struct {
	ID string `json:"id"`
	// File is the package's file name inside the scripts directory; empty
	// means <id>.flux. An app that filed an older install under another name
	// says so here, and the update replaces that very file.
	File      string   `json:"file,omitempty"`
	Version   string   `json:"version"`
	Wire      int      `json:"wire,omitempty"`
	PubkeyHex string   `json:"pubkey"`
	Update    []string `json:"update,omitempty"`
}

// UpdateReport is the result of a check or an apply.
type UpdateReport struct {
	Status string `json:"status"`
	Code   string `json:"code,omitempty"`
	// Current and Latest are versions; Wire is the wire generation of Latest.
	Current   string `json:"current,omitempty"`
	Latest    string `json:"latest,omitempty"`
	Wire      int    `json:"wire,omitempty"`
	WireBreak bool   `json:"wireBreak,omitempty"`
	Notes     string `json:"notes,omitempty"`
	// NewKey is set when the update was signed by another of the project's own
	// keys than the one the install was pinned to (a key rotation): the app
	// pins the install to it from now on.
	NewKey string `json:"newKey,omitempty"`
	// Official: signed by a first-party key. AutoOK: the app may install
	// without asking (official, same wire, same key). Everything else asks.
	Official bool `json:"official,omitempty"`
	AutoOK   bool `json:"autoOk,omitempty"`

	url, sha string
	mirrors  []string
}

// IndexChannel is one release line of an update index.
type IndexChannel struct {
	Version   string   `json:"version"`
	Wire      int      `json:"wire,omitempty"`
	API       int      `json:"api,omitempty"`
	URL       string   `json:"url"`
	Mirrors   []string `json:"mirrors,omitempty"`
	SHA256    string   `json:"sha256"`
	Notes     string   `json:"notes,omitempty"`
	PublicKey string   `json:"publicKey,omitempty"`
}

// UpdateIndex is the author's update.json.
type UpdateIndex struct {
	Format   int                     `json:"format"`
	ID       string                  `json:"id"`
	Channels map[string]IndexChannel `json:"channels"`
}

// FileName is the package's file name inside the scripts directory.
func (i Installed) FileName() string {
	if i.File != "" {
		return filepath.Base(i.File)
	}
	return i.ID + ".flux"
}

// Fetcher GETs a URL; the check and apply paths take it as a parameter so
// they can be tested and so a platform can route through its own client.
type Fetcher func(ctx context.Context, url string, limit int64) ([]byte, error)

// HTTPFetcher is the default Fetcher.
func HTTPFetcher(ctx context.Context, rawURL string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("answer larger than %d bytes", limit)
	}
	return b, nil
}

// SecureURL: https, or plain http only to the loopback (tests, local dev). The one rule
// for update addresses, in the core and in scriptsign.
func SecureURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	h := u.Hostname()
	return u.Scheme == "http" && (h == "localhost" || h == "127.0.0.1" || h == "::1")
}

// CheckUpdate asks the transport's update sources for the newest version on
// channel ("stable" when empty or missing from the index).
func CheckUpdate(ctx context.Context, inst Installed, channel string, get Fetcher) UpdateReport {
	rep := UpdateReport{Current: inst.Version}
	fail := func(code string) UpdateReport { rep.Status, rep.Code = UpdateError, code; return rep }

	if len(inst.Update) == 0 {
		return fail(CodeNoSource)
	}
	idx, base, code := fetchIndex(ctx, inst, get)
	if code != "" {
		return fail(code)
	}
	if channel == "" {
		channel = "stable"
	}
	ch, ok := idx.Channels[channel]
	if !ok {
		if ch, ok = idx.Channels["stable"]; !ok {
			return fail(CodeNoChannel)
		}
	}
	rep.Latest, rep.Wire, rep.Notes = ch.Version, effWire(ch.Wire), ch.Notes
	rep.Official = IsOfficialKey(inst.PubkeyHex)
	if !validVersion(ch.Version) {
		return fail(CodeBadIndex)
	}
	rep.url, rep.sha, rep.mirrors = resolve(base, ch.URL), strings.ToLower(ch.SHA256), resolveAll(base, ch.Mirrors)

	if CompareVersions(ch.Version, inst.Version) <= 0 {
		rep.Status = UpdateUpToDate
		return rep
	}
	rep.Status = UpdateAvailable
	rep.WireBreak = rep.Wire != effWire(inst.Wire)
	switch {
	case ch.API > APIVersion:
		rep.Status, rep.Code = UpdateBlocked, CodeNeedsNewerApp
	case ch.PublicKey != "" && !strings.EqualFold(strings.ReplaceAll(ch.PublicKey, " ", ""), inst.PubkeyHex) &&
		!(rep.Official && IsOfficialKey(ch.PublicKey)): // a rotation between the project's own keys is not a new trust decision
		rep.Status, rep.Code = UpdateBlocked, CodeKeyChanged
	case rep.WireBreak:
		rep.Code = CodeWireBreak
	default:
		rep.AutoOK = rep.Official
	}
	return rep
}

// ApplyUpdate checks, downloads, verifies and installs the update into dir
// (as inst.FileName(), the previous version kept as <file>.prev). A wire
// break is installed only when allowWireBreak says the caller has taken
// responsibility for the node side too.
func ApplyUpdate(ctx context.Context, inst Installed, channel, dir string, allowWireBreak bool, get Fetcher) UpdateReport {
	rep := CheckUpdate(ctx, inst, channel, get)
	if rep.Status != UpdateAvailable {
		return rep
	}
	if rep.WireBreak && !allowWireBreak {
		rep.Status = UpdateBlocked
		return rep
	}
	fail := func(code string) UpdateReport { rep.Status, rep.Code = UpdateError, code; return rep }

	var data []byte
	for _, u := range append([]string{rep.url}, rep.mirrors...) {
		if !SecureURL(u) {
			return fail(CodeInsecureURL)
		}
		b, err := get(ctx, u, maxPackageBytes)
		if err == nil {
			data = b
			break
		}
	}
	if data == nil {
		return fail(CodeFetchFailed)
	}
	code, newKey := verifyDownload(data, inst, rep)
	if code != "" {
		return fail(code)
	}
	rep.NewKey = newKey
	if err := InstallPackage(dir, inst.FileName(), data); err != nil {
		return fail(CodeInstallFailed)
	}
	rep.Status, rep.Code = UpdateInstalled, ""
	return rep
}

// verifyDownload is every check a downloaded package passes before it
// replaces anything; "" means it may be installed. The second result is the
// key that signed it when that is another of the project's own keys than the
// pinned one (a rotation), "" otherwise.
func verifyDownload(data []byte, inst Installed, rep UpdateReport) (string, string) {
	if rep.sha != "" {
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != rep.sha {
			return CodeBadHash, ""
		}
	}
	pkg, err := ReadPackage(data)
	if err != nil {
		return CodeBadPackage, ""
	}
	pinned := strings.ReplaceAll(inst.PubkeyHex, " ", "")
	signedBy := ""
	if pub, err := DecodePublicKeyHex(pinned); err == nil && pkg.Verify(pub) == nil {
		signedBy = pinned
	} else if IsOfficialKey(pinned) {
		// An official transport may move to another official key.
		for _, k := range officialKeys {
			if strings.EqualFold(k, pinned) {
				continue
			}
			if pub, err := DecodePublicKeyHex(k); err == nil && pkg.Verify(pub) == nil {
				signedBy = k
				break
			}
		}
	}
	if signedBy == "" {
		return CodeBadSignature, ""
	}
	m := pkg.Manifest
	if m.EffectiveID() != inst.ID {
		return CodeIDMismatch, ""
	}
	if m.Version != rep.Latest || m.EffectiveWire() != rep.Wire {
		return CodeVersionMismatch, ""
	}
	if m.EffectiveAPI() > APIVersion {
		return CodeNeedsNewerApp, ""
	}
	if CompareVersions(m.Version, inst.Version) <= 0 {
		return CodeNotNewer, ""
	}
	if !strings.EqualFold(signedBy, pinned) {
		return "", strings.ToLower(signedBy)
	}
	return "", ""
}

// InstallPackage writes data as dir/<file> atomically, moving the file it
// replaces to <file>.prev. The caller has verified data.
func InstallPackage(dir, file string, data []byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	cur := filepath.Join(dir, filepath.Base(file))
	tmp := cur + ".new"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if _, err := os.Stat(cur); err == nil {
		if err := os.Rename(cur, cur+".prev"); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	if err := os.Rename(tmp, cur); err != nil {
		// Put the old one back rather than leave the transport missing.
		_ = os.Rename(cur+".prev", cur)
		os.Remove(tmp)
		return err
	}
	return nil
}

// RollbackPackage makes the previous version current again (the version it
// replaces becomes the new "previous", so a rollback can itself be undone).
// The previous file is re-verified under the pinned key first: it fails
// closed like every other load.
func RollbackPackage(dir, file, pubkeyHex string) UpdateReport {
	cur := filepath.Join(dir, filepath.Base(file))
	prev := cur + ".prev"
	rep := UpdateReport{Status: UpdateError}
	pinned := strings.ReplaceAll(pubkeyHex, " ", "")
	pub, err := DecodePublicKeyHex(pinned)
	if err != nil {
		rep.Code = CodeBadSignature
		return rep
	}
	if _, err := os.Stat(prev); err != nil {
		rep.Code = CodeNoPrevious
		return rep
	}
	p, err := LoadSignedPackage(prev, pub)
	if err != nil && IsOfficialKey(pinned) {
		// The previous version may predate a rotation of the project's keys.
		for _, k := range officialKeys {
			if other, kerr := DecodePublicKeyHex(k); kerr == nil {
				if p, err = LoadSignedPackage(prev, other); err == nil {
					break
				}
			}
		}
	}
	if err != nil {
		rep.Code = CodeBadSignature
		return rep
	}
	tmp := cur + ".swap"
	if err := swap(cur, prev, tmp); err != nil {
		rep.Code = CodeInstallFailed
		return rep
	}
	rep.Status, rep.Latest, rep.Wire = UpdateInstalled, p.Manifest.Version, p.Manifest.EffectiveWire()
	return rep
}

func swap(cur, prev, tmp string) error {
	if _, err := os.Stat(cur); err != nil { // nothing current: just promote prev
		return os.Rename(prev, cur)
	}
	if err := os.Rename(cur, tmp); err != nil {
		return err
	}
	if err := os.Rename(prev, cur); err != nil {
		_ = os.Rename(tmp, cur)
		return err
	}
	return os.Rename(tmp, prev)
}

// fetchIndex tries each declared source in order and returns the first
// index that parses and belongs to this transport, with the URL it came from.
func fetchIndex(ctx context.Context, inst Installed, get Fetcher) (*UpdateIndex, string, string) {
	code := CodeFetchFailed
	for _, u := range inst.Update {
		if !SecureURL(u) {
			code = CodeInsecureURL
			continue
		}
		b, err := get(ctx, u, maxIndexBytes)
		if err != nil {
			continue
		}
		var idx UpdateIndex
		if err := json.NewDecoder(bytes.NewReader(b)).Decode(&idx); err != nil || idx.Format != indexFormat {
			code = CodeBadIndex
			continue
		}
		if idx.ID != inst.ID {
			code = CodeIDMismatch
			continue
		}
		return &idx, u, ""
	}
	return nil, "", code
}

func resolve(base, ref string) string {
	b, err := url.Parse(base)
	r, err2 := url.Parse(ref)
	if err != nil || err2 != nil {
		return ref
	}
	return b.ResolveReference(r).String()
}

func resolveAll(base string, refs []string) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, resolve(base, r))
	}
	return out
}

func effWire(w int) int {
	if w <= 0 {
		return 1
	}
	return w
}

// validVersion: MAJOR.MINOR.PATCH with an optional -prerelease.
func validVersion(v string) bool {
	_, _, ok := parseVersion(v)
	return ok
}

func parseVersion(v string) (core [3]int, pre []string, ok bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexByte(v, '+'); i >= 0 { // build metadata does not order
		v = v[:i]
	}
	if i := strings.IndexByte(v, '-'); i >= 0 {
		pre = strings.Split(v[i+1:], ".")
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return core, nil, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return core, nil, false
		}
		core[i] = n
	}
	return core, pre, true
}

// CompareVersions orders two semantic versions: -1, 0 or 1. A pre-release is
// lower than its release; an unparsable version is lower than any valid one.
func CompareVersions(a, b string) int {
	ac, ap, aok := parseVersion(a)
	bc, bp, bok := parseVersion(b)
	switch {
	case !aok && !bok:
		return 0
	case !aok:
		return -1
	case !bok:
		return 1
	}
	for i := range ac {
		if ac[i] != bc[i] {
			if ac[i] < bc[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(ap) == 0 && len(bp) == 0:
		return 0
	case len(ap) == 0:
		return 1
	case len(bp) == 0:
		return -1
	}
	for i := 0; i < len(ap) && i < len(bp); i++ {
		an, aerr := strconv.Atoi(ap[i])
		bn, berr := strconv.Atoi(bp[i])
		var c int
		switch {
		case aerr == nil && berr == nil:
			c = cmpInt(an, bn)
		case aerr == nil: // numeric identifiers are lower than alphanumeric
			c = -1
		case berr == nil:
			c = 1
		default:
			c = strings.Compare(ap[i], bp[i])
		}
		if c != 0 {
			return c
		}
	}
	return cmpInt(len(ap), len(bp))
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
