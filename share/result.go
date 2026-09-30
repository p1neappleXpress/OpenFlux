package share

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/p1neappleXpress/OpenFlux/transport"
)

// Result is what every entry point answers about a link, as the same JSON
// (the CLI's --parse-link and --make-link, package mobile for Android, the
// iOS library): the configuration and its context, or the link built, or
// why it could not be. Apps map Code (and Param) to their own words; Error
// is the English detail, for logs.
type Result struct {
	Config *Config `json:"config,omitempty"`
	// Context is the encryption context a client of this link derives.
	Context string `json:"context,omitempty"`
	Link    string `json:"link,omitempty"`
	Error   string `json:"error,omitempty"`
	Code    string `json:"code,omitempty"`
	Param   string `json:"param,omitempty"`
}

// JSON is r as the apps receive it.
func (r Result) JSON() string {
	b, err := json.Marshal(r)
	if err != nil {
		return `{"error":"marshal failed"}`
	}
	return string(b)
}

// Failed is the Result for err: its Code and Param when it is a link
// problem, just the text otherwise.
func Failed(err error) Result {
	r := Result{Error: err.Error()}
	var e *Error
	if errors.As(err, &e) {
		r.Code, r.Param = e.Code, e.Param
	}
	return r
}

// Read parses a link the way every client does and gives the context a
// client derives for it.
func Read(link string) Result {
	c, err := Decode(link)
	if err != nil {
		return Failed(err)
	}
	if c.Mode == ModeStream {
		return Result{Config: &c} // no encryption in stream mode: no context to derive
	}
	return Result{Config: &c, Context: ContextOf(c)}
}

// Make builds the link for c the way every client exports one, so the
// same configuration gives the same link on every client:
//
//   - addresses lose surrounding spaces;
//   - the default codec ("batched") is left out, and so is the priority
//     of a lone carrier (there is nothing to rank it against);
//   - an encrypted link always names its context: the one given, else the
//     one both peers derive (transport.KDFContexts); a link without a
//     secret carries none, there is nothing to derive.
//
// Result.Config is the configuration as it went into the link.
func Make(c Config) Result {
	c.Name = strings.TrimSpace(c.Name)
	c.Context = strings.TrimSpace(c.Context)
	if c.Codec == "batched" || c.Mode == ModeStream {
		c.Codec = "" // stream mode has no codec to choose
	}
	ts := make([]Transport, len(c.Transports))
	for i, t := range c.Transports {
		t.Type = strings.TrimSpace(t.Type)
		t.Name = strings.TrimSpace(t.Name)
		t.URL = strings.TrimSpace(t.URL)
		t.Dial = strings.TrimSpace(t.Dial)
		if t.Name == t.Type {
			t.Name = ""
		}
		ts[i] = t
	}
	if len(ts) == 1 {
		ts[0].Priority = 0
	}
	c.Transports = ts
	switch {
	case c.Secret == "":
		c.Context = ""
	case c.Context == "":
		c.Context = ContextOf(c)
	}
	link, err := Encode(c)
	if err != nil {
		return Failed(err)
	}
	return Result{Config: &c, Context: c.Context, Link: link}
}

// MakeJSON is Make for a configuration given as JSON (share.Config).
func MakeJSON(configJSON string) Result {
	var c Config
	if err := json.Unmarshal([]byte(configJSON), &c); err != nil {
		return Failed(linkError(CodeBadConfig, "", "share: bad configuration JSON", err))
	}
	return Make(c)
}

// ContextOf is the context a client derives for c: the link's own, else
// the one rule every peer uses.
func ContextOf(c Config) string {
	sources := make([]transport.ContextSource, len(c.Transports))
	for i, t := range c.Transports {
		sources[i] = transport.ContextSource{Type: t.Type, URL: t.URL, Priority: t.Priority}
	}
	context, _ := transport.KDFContexts(c.Context, "", sources)
	return context
}

// NodeConfig is the configuration of a channel the node wizard sets up
// (desktop and Android build it here, so their links are the same): the
// Yandex document first, direct to dial (host:port) as the backup, the
// document as the context, and the channel key.
func NodeConfig(name, documentURL, key, dial string) Config {
	return Config{
		Name:      name,
		Negotiate: true,
		Secret:    key,
		Context:   documentURL,
		Transports: []Transport{
			{Type: "vyandex", URL: documentURL, Priority: 100},
			{Type: "direct", Dial: dial, Priority: 50},
		},
	}
}

// MakeLink is Make returning just the link, or the error behind the
// Result (a *Error for a link problem).
func MakeLink(c Config) (string, error) {
	r := Make(c)
	if r.Error != "" {
		return "", r.err()
	}
	return r.Link, nil
}

// err turns a failed Result back into an error.
func (r Result) err() error {
	return &Error{Code: r.Code, Param: r.Param, text: r.Error}
}
