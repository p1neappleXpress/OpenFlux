// Package registry is the one place that knows how a transport type becomes a
// transport. The CLI's factory, the CLI's classic and stream paths and the
// phone bridges each used to carry their own switch over the same types, and
// the copies had drifted apart (direct took different parameters on a phone
// and on a computer, a block for script transports had been pasted twice, an
// empty type meant yandex in one and an error in another). Adding a transport,
// or swapping a native one for its JS package, is now one change here.
package registry

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/p1neappleXpress/OpenFlux/transport"
	"github.com/p1neappleXpress/OpenFlux/transport/cupsonline"
	"github.com/p1neappleXpress/OpenFlux/transport/mailru"
	"github.com/p1neappleXpress/OpenFlux/transport/oneme"
	"github.com/p1neappleXpress/OpenFlux/transport/script"
	"github.com/p1neappleXpress/OpenFlux/transport/yandex"
)

// Options is what differs between the callers; the type, url and params are
// the configuration of the carrier itself.
type Options struct {
	// Base is the transport configuration every carrier starts from.
	Base transport.TransportConfig
	// IsExit is this process's role: a cupsonline client never creates rooms,
	// oneme picks receiver or caller, direct listens or dials.
	IsExit bool
	// LowMemory is for phones (above all the iOS Network Extension): a shorter
	// queue and the slim Volga profile.
	LowMemory bool
	// YandexCookiesFile is the CLI's --yandex-cookies-file: a cookies.txt every
	// vyandex transport starts with.
	YandexCookiesFile string
	// StrictDirect makes a direct transport without an address an error (the
	// apps tell the user); without it the CLI's behavior stays: the transport
	// fails when it starts.
	StrictDirect bool
}

// The error texts are still the ones the apps showed before this package
// existed; moving every user-facing word out of the core is a separate step.

// Types are the transport types New accepts, in the order the apps list them.
var Types = []string{"yandex", "vyandex", "boards", "mailru", "cupsonline", "telemost", "oneme", "direct", "script"}

// New builds the raw carrier of the given type. params carries what a type
// needs beyond its url: "token"/"uid" (and "exit") for oneme, "dial"/"listen"
// (and "is_exit") for direct, "path"/"pubkey"/"name" plus whatever the script
// itself asked for for script. An empty type is yandex.
func New(typ, url string, params map[string]interface{}, o Options) (transport.Transport, error) {
	base := o.Base
	if o.LowMemory {
		base.MaxQueueSize = 512
	}
	str := func(key string) string {
		v, _ := params[key].(string)
		return v
	}
	switch typ {
	case "", "yandex":
		return yandex.NewYandexDocsTransport(url, base), nil
	case "vyandex":
		var t *yandex.YandexVolgaTransport
		if o.LowMemory {
			t = yandex.NewYandexVolgaTransportWithConfig(url, base, yandex.SlimVolgaConfig())
		} else {
			t = yandex.NewYandexVolgaTransport(url, base)
		}
		if o.YandexCookiesFile != "" {
			if err := t.LoadCookieFile(o.YandexCookiesFile); err != nil {
				return nil, err
			}
		}
		return t, nil
	case "boards":
		return yandex.NewBoardsTransport(url, base), nil
	case "mailru":
		return mailru.NewMailruDocsTransport(url, base), nil
	case "cupsonline":
		return cupsonline.NewCupsonlineTransport(url, base, !o.IsExit), nil
	case "oneme":
		uid, _ := strconv.ParseInt(str("uid"), 10, 64)
		return oneme.NewOneMeTransport(roleParam(params, "exit", o.IsExit), str("token"), uid, base), nil
	case "telemost":
		return newTelemost(url, params, o), nil
	case "script":
		return newScript(url, params, o)
	case "direct":
		return newDirect(params, o)
	}
	return nil, fmt.Errorf("unknown transport type %q", typ) // the apps never offer an unknown one
}

// newTelemost builds the Telemost carrier: a Yandex Telemost conference whose
// SFU relays the tunnel as video. One Telemost stream tops out near 2 MB/s,
// so the carrier always runs as a striped group of lanes (participants of the
// same conference). params:
//
//	"stripes"     a fixed lane count (default telemostLanes), or "auto": start
//	              at 3 and add lanes while they stay saturated
//	"stripes_max" upper bound for auto (default 11)
//	"cookies"     Yandex cookie header, only needed to create a conference
//
// Both ends of a tunnel stripe, so the two must be this build or later.
func newTelemost(url string, params map[string]interface{}, o Options) transport.Transport {
	str := func(key string) string {
		switch v := params[key].(type) {
		case string:
			return v
		case float64:
			return strconv.Itoa(int(v))
		case int:
			return strconv.Itoa(v)
		}
		return ""
	}
	n, auto := telemostLanes, false
	st := str("stripes")
	if e := os.Getenv("TELEMOST_STRIPES"); e != "" { // debugging override
		st = e
	}
	switch {
	case st == "auto":
		n, auto = 3, true
	case st != "":
		if v, err := strconv.Atoi(st); err == nil && v >= 1 {
			n = v
		}
	}
	maxLanes, _ := strconv.Atoi(str("stripes_max"))
	if maxLanes < n {
		maxLanes = 11
	}
	g := yandex.NewTelemostGroup(str("cookies"), url, n, o.IsExit, o.Base)
	return transport.NewStripedTransportAuto(g.Lanes(), g, maxLanes, auto)
}

// telemostLanes is the lane count of a Telemost carrier unless its profile
// says otherwise: five streams, about five times what one carries. Both ends
// run the same count, so the two sides of a tunnel pair lane for lane.
const telemostLanes = 5

// roleParam reads a role flag a profile may carry as a bool or as "true".
func roleParam(params map[string]interface{}, key string, def bool) bool {
	switch v := params[key].(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(v, "true")
	}
	return def
}

func newDirect(params map[string]interface{}, o Options) (transport.Transport, error) {
	str := func(key string) string {
		v, _ := params[key].(string)
		return v
	}
	dcfg := transport.DefaultDirectConfig()
	dcfg.IsExit = roleParam(params, "is_exit", o.IsExit)
	dial, listen := str("dial"), str("listen")
	if dcfg.IsExit && listen == "" {
		listen = dial // a profile keeps one address per transport
	}
	dcfg.DialAddr, dcfg.ListenAddr = dial, listen
	if o.StrictDirect {
		if dcfg.IsExit && listen == "" {
			return nil, errors.New("direct: не указан адрес для прослушивания (host:port)")
		}
		if !dcfg.IsExit && dial == "" {
			return nil, errors.New("direct: не указан адрес ноды (host:port)")
		}
	}
	return transport.NewDirectTransport(o.Base, dcfg), nil
}

// newScript builds a JS (goja) script transport. params carries the on-disk
// script ("path" to <name>.flux or <name>.js, its signature verified against
// the pinned "pubkey") plus whatever the script's own info().params asked the
// operator for. script.New verifies the signature before the script is ever
// evaluated, so a swapped file on disk fails closed here, not at connect. The
// core adds the role it knows and the script cannot (see withRole).
func newScript(url string, params map[string]interface{}, o Options) (transport.Transport, error) {
	str := func(key string) string {
		v, _ := params[key].(string)
		return v
	}
	path := str("path")
	if path == "" {
		return nil, errors.New("script: не указан путь к скрипту")
	}
	pub, err := script.DecodePublicKeyHex(str("pubkey"))
	if err != nil {
		return nil, fmt.Errorf("script: ключ автора: %w", err)
	}
	name := str("name")
	if name == "" {
		name = "script"
	}
	base := o.Base
	if o.LowMemory {
		base.MaxQueueSize = 512
	}
	return script.New(name, path, pub, url, withRole(withSettings(params), o.IsExit), base)
}

// coreParams are the script params the core owns: a setting cannot shadow them.
var coreParams = map[string]bool{"path": true, "pubkey": true, "name": true, "exit": true, "settings": true}

// withSettings flattens params["settings"] (the values the user saved in the
// script's settings wizard; the .conf carries them as one encoded line, see
// DecodeSettings) into the params the script sees as cfg.params, next to the
// ones the core adds. A setting never overrides a core-owned key.
func withSettings(params map[string]interface{}) map[string]interface{} {
	settings, ok := params["settings"].(map[string]interface{})
	if !ok {
		return params
	}
	out := make(map[string]interface{}, len(params)+len(settings))
	for k, v := range params {
		if k != "settings" {
			out[k] = v
		}
	}
	for k, v := range settings {
		if !coreParams[k] {
			out[k] = v
		}
	}
	return out
}

// withRole copies a script transport's params and adds the role the core
// knows and the script cannot: "exit" (a cupsonline exit creates rooms, a
// client never does; oneme picks caller or receiver). A value the profile
// already set is kept.
func withRole(params map[string]interface{}, isExit bool) map[string]interface{} {
	out := make(map[string]interface{}, len(params)+1)
	for k, v := range params {
		out[k] = v
	}
	if _, set := out["exit"]; !set {
		out["exit"] = isExit
	}
	return out
}
