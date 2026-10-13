// Command opensub is the OpenFlux subscription service and its owner commands.
//
//	opensub serve                       run the service (systemd)
//	opensub status                      certificate pin, locations, whether they read
//	opensub users | grant <login> <plan|days> | ban <login> | unban <login>
//	opensub reset-password <login> | delete-user <login>
//	opensub key-add <name> [days] [pools] | keys | key-extend <id> <days> | key-revoke <id>
//	opensub orders | paid <order>
//	opensub rotate <pool>               new key for a pool's fixed channel (root; not for per-user pools)
//	opensub config-example
//
// The commands call the service's admin API on this machine; see
// docs/subscriptions.ru.md.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/p1neappleXpress/OpenFlux/subscription/server"
)

const defaultConfig = "/etc/openflux-sub/config.json"

func main() {
	args := os.Args[1:]
	cfgPath := os.Getenv("OPENSUB_CONFIG")
	if cfgPath == "" {
		cfgPath = defaultConfig
	}
	for len(args) >= 2 && args[0] == "-c" {
		cfgPath, args = args[1], args[2:]
	}
	if len(args) == 0 {
		usage()
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		usage()
	}
	if args[0] == "config-example" {
		fmt.Print(exampleConfig)
		return
	}
	cfg, err := server.LoadConfig(cfgPath)
	if err != nil {
		fatal(err)
	}
	cmd, rest := args[0], args[1:]
	// A flag is never a login or a key's name: "key-add --help" must not make a key called "--help".
	for _, a := range rest {
		if strings.HasPrefix(a, "-") {
			usage()
		}
	}
	switch cmd {
	case "serve":
		serve(cfg)
	case "rotate":
		need(rest, 1, "rotate <pool>")
		rotate(cfg, rest[0])
	default:
		admin(cfg, cmd, rest)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: opensub [-c config] serve | status | users | grant <login> <plan|days> | ban|unban|reset-password|delete-user <login> |")
	fmt.Fprintln(os.Stderr, "       key-add <name> [days] [pools] | keys | key-extend <id> <days> | key-revoke <id> | orders | paid <order> | rotate <pool> | config-example")
	os.Exit(2)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "opensub:", err)
	os.Exit(1)
}

func need(args []string, n int, form string) {
	if len(args) < n {
		fatal(fmt.Errorf("usage: opensub %s", form))
	}
}

func serve(cfg *server.Config) {
	s, err := server.New(cfg, server.Options{})
	if err != nil {
		fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := s.Serve(ctx); err != nil {
		fatal(err)
	}
}

// api calls the admin API of the service on this machine. It is the loopback
// address, so the certificate (self-signed or for a domain) is not checked.
func api(cfg *server.Config, method, path string, body any) (int, []byte) {
	_, port, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		port = "8443"
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "https://127.0.0.1:"+port+path, rd)
	req.Header.Set("Authorization", "Bearer "+cfg.AdminToken)
	cl := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	resp, err := cl.Do(req)
	if err != nil {
		fatal(fmt.Errorf("the service does not answer on port %s (is it running? systemctl status openflux-sub): %w", port, err))
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func call(cfg *server.Config, method, path string, body any) map[string]any {
	code, b := api(cfg, method, path, body)
	var out map[string]any
	json.Unmarshal(b, &out)
	if code >= 400 {
		msg, _ := out["message"].(string)
		fatal(fmt.Errorf("%d %s", code, msg))
	}
	return out
}

func admin(cfg *server.Config, cmd string, a []string) {
	switch cmd {
	case "status":
		info := call(cfg, "GET", "/admin/info", nil)
		fmt.Printf("service: https://%v\ncertificate pin: sha256:%v (self-signed: %v)\n", info["hostPort"], info["pin"], info["selfSigned"])
		for _, p := range info["pools"].([]any) {
			pl := p.(map[string]any)
			state := "ok"
			if ok, _ := pl["OK"].(bool); !ok {
				state = fmt.Sprint("BROKEN: ", pl["Error"])
			}
			fmt.Printf("  pool %-10v channel %-12v %s\n", pl["ID"], pl["Channel"], state)
		}
	case "users":
		code, b := api(cfg, "GET", "/admin/users", nil)
		must(code, b)
		var users []map[string]any
		json.Unmarshal(b, &users)
		fmt.Printf("%-24s %-8s %-8s %-17s %s\n", "LOGIN", "STATUS", "PLAN", "EXPIRES", "DEVICES")
		for _, u := range users {
			fmt.Printf("%-24v %-8v %-8v %-17s %v\n", u["login"], u["status"], u["plan"], when(u["expires"]), u["devices"])
		}
	case "grant":
		need(a, 2, "grant <login> <plan|days>")
		body := map[string]any{}
		if n, err := strconv.Atoi(a[1]); err == nil {
			body["days"] = n
		} else {
			body["planId"] = a[1]
		}
		u := call(cfg, "POST", "/admin/users/"+a[0]+"/grant", body)
		fmt.Printf("%v: %v until %s\n", u["login"], u["status"], when(u["expires"]))
	case "ban", "unban":
		need(a, 1, cmd+" <login>")
		call(cfg, "POST", "/admin/users/"+a[0]+"/ban", map[string]any{"banned": cmd == "ban"})
		fmt.Println(a[0], cmd+"ned")
	case "reset-password":
		need(a, 1, "reset-password <login>")
		r := call(cfg, "POST", "/admin/users/"+a[0]+"/reset-password", nil)
		fmt.Printf("temporary password for %v: %v\n(every device is signed out; tell the person to change it)\n", r["login"], r["password"])
	case "delete-user":
		need(a, 1, "delete-user <login>")
		call(cfg, "DELETE", "/admin/users/"+a[0], nil)
		fmt.Println("deleted", a[0])
	case "key-add":
		need(a, 1, "key-add <name> [days] [pool,pool]")
		body := map[string]any{"name": a[0]}
		if len(a) > 1 {
			n, err := strconv.Atoi(a[1])
			if err != nil {
				fatal(fmt.Errorf("days must be a number (0 is for ever)"))
			}
			body["days"] = n
		}
		if len(a) > 2 {
			body["pools"] = strings.Split(a[2], ",")
		}
		k := call(cfg, "POST", "/admin/keys", body)
		fmt.Printf("key %v (shown once)\n%v\n", k["id"], k["invite"])
	case "keys":
		code, b := api(cfg, "GET", "/admin/keys", nil)
		must(code, b)
		var keys []map[string]any
		json.Unmarshal(b, &keys)
		for _, k := range keys {
			state := "active"
			if r, _ := k["revoked"].(bool); r {
				state = "revoked"
			}
			fmt.Printf("%-14v %-20v %-8s %s\n", k["id"], k["name"], state, when(k["expires"]))
		}
	case "key-extend":
		need(a, 2, "key-extend <id> <days>")
		n, err := strconv.Atoi(a[1])
		if err != nil {
			fatal(err)
		}
		k := call(cfg, "POST", "/admin/keys/"+a[0]+"/extend", map[string]any{"days": n})
		fmt.Println("until", when(k["expires"]))
	case "key-revoke":
		need(a, 1, "key-revoke <id>")
		call(cfg, "DELETE", "/admin/keys/"+a[0], nil)
		fmt.Println("revoked", a[0])
	case "orders":
		code, b := api(cfg, "GET", "/admin/orders", nil)
		must(code, b)
		var orders []map[string]any
		json.Unmarshal(b, &orders)
		for _, o := range orders {
			fmt.Printf("%-14v %-20v %-8v %-8v %s\n", o["id"], o["login"], o["planId"], o["status"], when(o["createdAt"]))
		}
	case "paid":
		need(a, 1, "paid <order>")
		r := call(cfg, "POST", "/admin/orders/"+a[0]+"/paid", nil)
		fmt.Printf("order %v %v, the account is active until %s\n", r["orderId"], r["status"], when(r["expires"]))
	default:
		usage()
	}
}

func must(code int, b []byte) {
	if code >= 400 {
		fatal(fmt.Errorf("%d %s", code, b))
	}
}

func when(v any) string {
	f, _ := v.(float64)
	if f == 0 {
		return "never"
	}
	return time.Unix(int64(f), 0).Format("2006-01-02 15:04")
}

// rotate gives a location's channel a new key and restarts its node. Every link
// made for the channel stops working; subscribers get the new one on their next
// refresh, anybody removed does not. Needs the rights to write the channel's
// key and to restart its unit (root).
func rotate(cfg *server.Config, poolID string) {
	for _, p := range cfg.Pools {
		if p.ID != poolID {
			continue
		}
		if p.PerUser {
			fatal(fmt.Errorf("pool %q: a per-user pool has no fixed channel to rotate; restart the openflux-node@u<hex> units themselves, one per subscriber", poolID))
		}
		if p.Channel == "" {
			fatal(fmt.Errorf("pool %q has no channel", poolID))
		}
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			fatal(err)
		}
		path := filepath.Join(cfg.NodeConfDir, p.Channel, "encryption-key")
		// Written in place: the owner and mode node-install.sh set stay.
		if err := os.WriteFile(path, []byte(hex.EncodeToString(key)+"\n"), 0o640); err != nil {
			fatal(err)
		}
		out, err := exec.Command("systemctl", "restart", "openflux-node@"+p.Channel).CombinedOutput()
		if err != nil {
			fatal(fmt.Errorf("the key is changed but the node did not restart: %v: %s", err, out))
		}
		fmt.Printf("pool %s: new key, node %s restarted. Subscribers get the new link at their next refresh;\nprofiles of this channel kept elsewhere (an imported link, the owner's own profile) need importing again.\n", p.ID, p.Channel)
		return
	}
	fatal(fmt.Errorf("no pool %q in the config", poolID))
}

const exampleConfig = `{
  "listen": ":8443",
  "publicHost": "203.0.113.5",
  "adminToken": "change-me-to-a-long-random-string",
  "dataDir": "/var/lib/openflux-sub",
  "title": "OpenFlux",
  "support": "https://t.me/your_support",
  "updateHours": 6,
  "autoTrial": true,
  "pools": [
    { "id": "main", "name": "Основной", "channel": "fork-all" }
  ],
  "plans": [
    { "id": "trial", "name": "Пробный", "days": 3, "devices": 1, "trial": true },
    { "id": "month", "name": "Месяц", "days": 30, "devices": 3, "price": "299 ₽" }
  ],
  "payment": { "provider": "manual", "instructions": "Напишите в поддержку, пришлите логин и чек." }
}
`
