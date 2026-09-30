// Package phphost puts the phpbox exit (deploy/phpbox) on an ordinary web
// host over FTP and checks that it works: probe the account, upload the
// bundle embedded in the core, ask the site whether it runs, start the node.
// It is what the apps' node wizard calls for "a free PHP hosting as the exit",
// so the steps, and every decision in them, exist once, here.
//
// Like the rest of the core it answers with codes and parameters (Error.Code,
// Error.Param), never with text for people: the apps word them. Secrets (the
// FTP password, the node's token) never appear in errors or logs.
package phphost

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
)

// Why a step failed (Error.Code). The apps word each one.
const (
	CodeBadParams       = "bad_params"
	CodeFTPConnect      = "ftp_connect"     // Param: host:port
	CodeFTPLogin        = "ftp_login"       // the server refused the user or password
	CodeFTPTLS          = "ftp_tls"         // the TLS the server asked for could not be set up
	CodeNoWebRoot       = "ftp_no_webroot"  // Param: the directories found, comma separated
	CodeDirMissing      = "ftp_dir_missing" // Param: the directory asked for
	CodeNotWritable     = "ftp_not_writable"
	CodeUpload          = "ftp_upload"       // Param: the file
	CodeSiteUnreachable = "site_unreachable" // the site did not answer
	CodeAntiBot         = "site_antibot"     // the host puts a browser check in front of the site
	CodeNotPhpbox       = "site_not_phpbox"  // the site answers, but not with the node: wrong address or folder, or PHP is off
	CodeTokenRefused    = "site_token"
	CodePHPMissing      = "php_missing" // Param: the functions the node needs and the host lacks
	CodeNodeNotStarted  = "node_not_started"
)

// Error is a failed step: Code says which (for the apps), Param what it is
// about, Error() the English detail for logs.
type Error struct {
	Code  string
	Param string
	text  string
	cause error
}

func (e *Error) Error() string {
	if e.cause != nil {
		return e.text + ": " + e.cause.Error()
	}
	return e.text
}
func (e *Error) Unwrap() error { return e.cause }

func fail(code, param, text string, cause error) error {
	return &Error{Code: code, Param: param, text: text, cause: cause}
}

// Result is what every entry point answers, as JSON: the payload, or why not.
type Result struct {
	OK    bool        `json:"ok"`
	Data  interface{} `json:"data,omitempty"`
	Error string      `json:"error,omitempty"`
	Code  string      `json:"code,omitempty"`
	Param string      `json:"param,omitempty"`
}

// JSON renders r.
func (r Result) JSON() string {
	b, err := json.Marshal(r)
	if err != nil {
		return `{"ok":false,"error":"marshal failed"}`
	}
	return string(b)
}

// Done is the Result of a step that worked.
func Done(data interface{}) Result { return Result{OK: true, Data: data} }

// Failed is the Result for err: its Code and Param when it is one of ours.
func Failed(err error) Result {
	r := Result{Error: err.Error()}
	var e *Error
	if errors.As(err, &e) {
		r.Code, r.Param = e.Code, e.Param
	}
	return r
}

// NewToken makes the secret that guards a node's pages.
func NewToken() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
