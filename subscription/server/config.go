// Package server is the subscription service (opensub): accounts, devices,
// plans, orders and the list of locations a subscriber may connect through.
// See docs/subscriptions.ru.md for the whole design.
package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/p1neappleXpress/OpenFlux/provision"
)

// Config is /etc/openflux-sub/config.json.
type Config struct {
	// Listen is the address the HTTPS service listens on, ":8443".
	Listen string `json:"listen"`
	// PublicHost is the address clients reach the service at (IP or domain); it
	// goes into the certificate and into keys.
	PublicHost string `json:"publicHost"`
	// AdminToken guards /admin; the opensub commands read it from here.
	AdminToken string `json:"adminToken"`
	// DataDir holds state.json and, for a self-signed certificate, cert.pem and key.pem.
	DataDir string `json:"dataDir"`
	// CertFile and KeyFile are a real certificate (a domain); empty makes a
	// self-signed one in DataDir, which apps trust by its pin.
	CertFile string `json:"certFile,omitempty"`
	KeyFile  string `json:"keyFile,omitempty"`
	// NodeConfDir is where node-install.sh keeps its channels.
	NodeConfDir string `json:"nodeConfDir,omitempty"`
	// NodeScript is node-install.sh on this machine (local, root), which the
	// autoNode pools run to create their channel.
	NodeScript string `json:"nodeScript,omitempty"`

	Title       string `json:"title"`
	Support     string `json:"support,omitempty"`
	Announce    string `json:"announce,omitempty"`
	UpdateHours int    `json:"updateHours,omitempty"`
	// AutoTrial gives a new account the trial plan at once.
	AutoTrial bool `json:"autoTrial"`

	Pools []Pool `json:"pools"`
	Plans []Plan `json:"plans"`
	// Payment describes how people pay; "manual" waits for the owner to confirm.
	Payment Payment `json:"payment"`

	// BcryptCost is for tests; 0 means 12.
	BcryptCost int `json:"bcryptCost,omitempty"`
}

// Pool is one location: a channel of a node installed by node-install.sh.
type Pool struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Channel string `json:"channel"`
	// Host is the node's address in the link's direct carrier (default PublicHost).
	Host string `json:"host,omitempty"`
	// AutoNode makes the service itself create the channel on this host when
	// its files are missing (opensub must run as root; see docs).
	AutoNode bool `json:"autoNode,omitempty"`
	// PerUser makes every owner (account or key) get their own channel, built
	// from this pool's transports template. The channel name is derived from
	// the owner, so there is no fixed Channel.
	PerUser bool `json:"perUser,omitempty"`
	// Transports are the channel's carriers besides direct (see provision.ChannelTransport).
	// A cupsonline entry with an empty url means "create the rooms yourself" (AutoNode);
	// empty Transports + AutoNode likewise gets cups.online rooms only.
	Transports []provision.ChannelTransport `json:"transports,omitempty"`
	// Mode is the node's forwarding mode: "l4" (default) or "l3".
	Mode string `json:"mode,omitempty"`
}

// Plan is what a subscriber can have.
type Plan struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Days    int    `json:"days"`
	Devices int    `json:"devices"`
	// Price is shown as is ("299 ₽"); the service does not bill.
	Price string `json:"price,omitempty"`
	// Trial plans are not for sale: a new account gets one once.
	Trial bool `json:"trial,omitempty"`
	// Pools limits the plan's locations (ids); empty is all.
	Pools []string `json:"pools,omitempty"`
}

// Payment is how orders are paid.
type Payment struct {
	Provider     string `json:"provider"`
	Instructions string `json:"instructions,omitempty"`
}

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// scriptChannelRe is node-install.sh's valid_channel: autoNode runs the script
// with the pool's channel name.
var scriptChannelRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)

// LoadConfig reads and checks the config file.
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, c.Check()
}

// Check validates the config and fills the defaults.
func (c *Config) Check() error {
	if c.Listen == "" {
		c.Listen = ":8443"
	}
	if c.PublicHost == "" {
		return errors.New("config: publicHost is required")
	}
	if len(c.AdminToken) < 24 {
		return errors.New("config: adminToken must be at least 24 characters")
	}
	if c.DataDir == "" {
		c.DataDir = "/var/lib/openflux-sub"
	}
	if c.NodeConfDir == "" {
		c.NodeConfDir = "/etc/openflux-node"
	}
	if c.NodeScript == "" {
		c.NodeScript = "/opt/openflux-node/node-install.sh"
	}
	if c.Title == "" {
		c.Title = "OpenFlux"
	}
	if c.UpdateHours <= 0 {
		c.UpdateHours = 6
	}
	if c.Payment.Provider == "" {
		c.Payment.Provider = "manual"
	}
	if c.Payment.Provider != "manual" {
		return fmt.Errorf("config: payment provider %q is not available (manual only)", c.Payment.Provider)
	}
	if len(c.Pools) == 0 {
		return errors.New("config: at least one pool is required")
	}
	pools := map[string]bool{}
	for i := range c.Pools {
		p := &c.Pools[i]
		if !idRe.MatchString(p.ID) || p.Name == "" {
			return fmt.Errorf("config: pool %q: id (a-z0-9-_) and name are required", p.ID)
		}
		if pools[p.ID] {
			return fmt.Errorf("config: pool %q twice", p.ID)
		}
		pools[p.ID] = true
		if p.Host == "" {
			p.Host = c.PublicHost
		}
		if p.PerUser {
			p.Transports = withoutDirect(p.Transports)
			if _, err := provision.CheckMode(p.Mode); err != nil {
				return fmt.Errorf("config: pool %q: %w", p.ID, err)
			}
			if err := checkAutoTransports(p.Transports); err != nil {
				return fmt.Errorf("config: pool %q: %w", p.ID, err)
			}
			continue
		}
		if !idRe.MatchString(p.Channel) {
			return fmt.Errorf("config: pool %q: id, name and channel (a-z0-9-_) are required", p.ID)
		}
		if p.AutoNode {
			if !scriptChannelRe.MatchString(p.Channel) {
				return fmt.Errorf("config: pool %q: autoNode wants a channel name like node-install.sh: a-z0-9 и дефис, до 31 символа", p.ID)
			}
			if _, err := provision.CheckMode(p.Mode); err != nil {
				return fmt.Errorf("config: pool %q: %w", p.ID, err)
			}
			if err := checkAutoTransports(p.Transports); err != nil {
				return fmt.Errorf("config: pool %q: %w", p.ID, err)
			}
		} else if hasEmptyCups(p.Transports) {
			return fmt.Errorf("config: pool %q: cupsonline или avito без url требует autoNode", p.ID)
		}
	}
	if len(c.Plans) == 0 {
		return errors.New("config: at least one plan is required")
	}
	plans := map[string]bool{}
	for _, p := range c.Plans {
		if !idRe.MatchString(p.ID) || p.Name == "" || p.Days < 1 || p.Devices < 1 {
			return fmt.Errorf("config: plan %q: id, name, days and devices are required", p.ID)
		}
		if plans[p.ID] {
			return fmt.Errorf("config: plan %q twice", p.ID)
		}
		plans[p.ID] = true
		for _, id := range p.Pools {
			if !pools[id] {
				return fmt.Errorf("config: plan %q: unknown pool %q", p.ID, id)
			}
		}
	}
	return nil
}

func (c *Config) plan(id string) (Plan, bool) {
	for _, p := range c.Plans {
		if p.ID == id {
			return p, true
		}
	}
	return Plan{}, false
}

func (c *Config) trialPlan() (Plan, bool) {
	for _, p := range c.Plans {
		if p.Trial {
			return p, true
		}
	}
	return Plan{}, false
}

// withoutDirect drops the implicit direct carrier from a template: every
// channel already has direct (its own port), and node-install.sh and
// provision.CheckTransports do not take it as a listed transport.
func withoutDirect(ts []provision.ChannelTransport) []provision.ChannelTransport {
	out := make([]provision.ChannelTransport, 0, len(ts))
	for _, t := range ts {
		if strings.TrimSpace(t.Type) == "direct" {
			continue
		}
		out = append(out, t)
	}
	return out
}

// selfMade: a room carrier (cups.online, Avito Code) without its rooms, which
// the service makes for each channel without any sign-in.
func selfMade(t provision.ChannelTransport) bool {
	return (t.Type == "cupsonline" || t.Type == "avito") && strings.TrimSpace(t.URL) == ""
}

// hasEmptyCups reports a room carrier that the service would create rooms
// for: only possible with autoNode.
func hasEmptyCups(ts []provision.ChannelTransport) bool {
	for _, t := range ts {
		if selfMade(t) {
			return true
		}
	}
	return false
}

// checkAutoTransports validates the transports of an autoNode pool: any type
// except a room carrier without rooms (selfMade) is checked like provision
// does; two of a type are refused.
func checkAutoTransports(ts []provision.ChannelTransport) error {
	seen := map[string]bool{}
	for _, t := range ts {
		if selfMade(t) {
			t.Type = strings.TrimSpace(t.Type)
		} else if _, err := provision.CheckTransports([]provision.ChannelTransport{t}); err != nil {
			return err
		}
		if seen[t.Type] {
			return fmt.Errorf("транспорт %s выбран дважды", t.Type)
		}
		seen[t.Type] = true
	}
	return nil
}

// userChannel is the personal channel name for an owner (account or key id):
// "u" and 18 hex of sha256(owner), which node-install.sh accepts.
func userChannel(owner string) string {
	sum := sha256.Sum256([]byte(owner))
	return "u" + hex.EncodeToString(sum[:9])
}
