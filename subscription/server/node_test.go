package server

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/p1neappleXpress/OpenFlux/provision"
)

// modeOf: node-install.sh.argv is [script plan] or [script apply conf].
func modeOf(args []string) string {
	if len(args) >= 2 {
		return args[1]
	}
	return ""
}

func TestScriptProvisionerCallsPlanThenApply(t *testing.T) {
	cfg := &Config{NodeScript: "/opt/openflux-node/node-install.sh"}
	var planConf, applyConf string
	var cmds []string
	sp := &scriptProvisioner{
		cfg:  cfg,
		logf: func(string, ...any) {},
		exec: func(_ context.Context, stdin string, name string, args ...string) ([]byte, error) {
			if name != "sh" {
				t.Fatalf("name %q, want sh", name)
			}
			switch modeOf(args) {
			case "plan":
				cmds = append(cmds, "plan")
				planConf = stdin
				return []byte(`{"ok":true,"channel":"fork-all","port":23234,"actions":["x"]}` + "\n"), nil
			case "apply":
				cmds = append(cmds, "apply")
				b, err := os.ReadFile(args[len(args)-1])
				if err != nil {
					t.Fatal(err)
				}
				applyConf = string(b)
				return []byte(`{"ok":true,"channel":"fork-all","port":23234}` + "\n"), nil
			}
			return nil, nil
		},
		rooms: func(context.Context) (string, error) { return "WyJyb29tMSIsInJvb20yIl0", nil },
	}
	p := Pool{ID: "main", Channel: "fork-all", AutoNode: true,
		Transports: []provision.ChannelTransport{{Type: "cupsonline", URL: ""}}}
	if err := sp.Ensure(context.Background(), nodeRequest{Pool: p, Channel: "fork-all"}); err != nil {
		t.Fatal(err)
	}
	if len(cmds) != 2 || cmds[0] != "plan" || cmds[1] != "apply" {
		t.Fatalf("commands %v, want plan then apply", cmds)
	}
	for _, want := range []string{"channel=fork-all", "cupsonline=WyJyb29tMSIsInJvb20yIl0", "autoupdate=no"} {
		if !strings.Contains(planConf, want) {
			t.Fatalf("plan conf misses %q: %q", want, planConf)
		}
	}
	if strings.Contains(planConf, "key=") {
		t.Fatal("plan must not carry the key")
	}
	for _, want := range []string{"key=", "port=23234"} {
		if !strings.Contains(applyConf, want) {
			t.Fatalf("apply conf misses %q: %q", want, applyConf)
		}
	}
	for _, line := range strings.Split(applyConf, "\n") {
		if strings.HasPrefix(line, "key=") && len(strings.TrimPrefix(line, "key=")) != 64 {
			t.Fatalf("key length: %q", line)
		}
	}
}

func TestScriptProvisionerEmptyTransportsMakeCupsRooms(t *testing.T) {
	var applyConf string
	sp := &scriptProvisioner{
		cfg:  &Config{NodeScript: "/node-install.sh"},
		logf: func(string, ...any) {},
		exec: func(_ context.Context, stdin string, name string, args ...string) ([]byte, error) {
			if name != "sh" {
				t.Fatalf("name %q, want sh", name)
			}
			switch modeOf(args) {
			case "plan":
				return []byte(`{"ok":true,"port":30001}` + "\n"), nil
			case "apply":
				b, _ := os.ReadFile(args[len(args)-1])
				applyConf = string(b)
				return []byte(`{"ok":true}` + "\n"), nil
			}
			return nil, nil
		},
		rooms: func(context.Context) (string, error) { return "WyJyb29tIl0", nil },
	}
	if err := sp.Ensure(context.Background(), nodeRequest{Pool: Pool{ID: "m", Channel: "fork-all", AutoNode: true}, Channel: "fork-all"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(applyConf, "cupsonline=WyJyb29tIl0") {
		t.Fatalf("roomless autoNode pool must get cups rooms: %q", applyConf)
	}
}

func TestScriptProvisionerReportsScriptErrors(t *testing.T) {
	sp := &scriptProvisioner{
		cfg:  &Config{NodeScript: "/node-install.sh"},
		logf: func(string, ...any) {},
		exec: func(_ context.Context, stdin string, name string, args ...string) ([]byte, error) {
			if name != "sh" {
				t.Fatalf("name %q, want sh", name)
			}
			if modeOf(args) == "plan" {
				return []byte(`{"ok":false,"error":"имя канала занято"}` + "\n"), nil
			}
			return nil, nil
		},
		rooms: func(context.Context) (string, error) { return "WyJyb29tIl0", nil },
	}
	err := sp.Ensure(context.Background(), nodeRequest{Pool: Pool{ID: "m", Channel: "fork-all", AutoNode: true,
		Transports: []provision.ChannelTransport{{Type: "cupsonline", URL: "WyJyb29tIl0"}}}, Channel: "fork-all"})
	if err == nil || !strings.Contains(err.Error(), "имя канала занято") {
		t.Fatalf("err %v", err)
	}
}

func TestScriptProvisionerUsesRequestChannel(t *testing.T) {
	var applyConf string
	sp := &scriptProvisioner{
		cfg:  &Config{NodeScript: "/node-install.sh"},
		logf: func(string, ...any) {},
		exec: func(_ context.Context, stdin string, name string, args ...string) ([]byte, error) {
			if name != "sh" {
				t.Fatalf("name %q, want sh", name)
			}
			switch modeOf(args) {
			case "plan":
				return []byte(`{"ok":true,"port":30001}` + "\n"), nil
			case "apply":
				b, _ := os.ReadFile(args[len(args)-1])
				applyConf = string(b)
				return []byte(`{"ok":true}` + "\n"), nil
			}
			return nil, nil
		},
		rooms: func(context.Context) (string, error) { return "WyJyb29tIl0", nil },
	}
	r := nodeRequest{Pool: Pool{ID: "main", Name: "M", PerUser: true}, Channel: "u1a2b3c4d5e6f708090", Owner: "acc-1"}
	if err := sp.Ensure(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(applyConf, "channel=u1a2b3c4d5e6f708090") {
		t.Fatalf("apply must use the request's channel, got: %q", applyConf)
	}
}

func TestScriptProvisionerMissingScript(t *testing.T) {
	sp := &scriptProvisioner{
		cfg:  &Config{NodeScript: "/nonexistent/node-install.sh"},
		logf: func(string, ...any) {},
		exec: func(_ context.Context, stdin string, name string, args ...string) ([]byte, error) {
			return nil, os.ErrNotExist
		},
		rooms: func(context.Context) (string, error) { return "WyJyb29tIl0", nil },
	}
	err := sp.Ensure(context.Background(), nodeRequest{Pool: Pool{ID: "m", Channel: "fork-all", AutoNode: true,
		Transports: []provision.ChannelTransport{{Type: "cupsonline", URL: "WyJyb29tIl0"}}}, Channel: "fork-all"})
	if err == nil || !strings.Contains(err.Error(), "plan:") {
		t.Fatalf("err %v", err)
	}
}