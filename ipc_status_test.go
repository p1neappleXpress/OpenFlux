package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport"
	"github.com/p1neappleXpress/OpenFlux/transport/ipc"
)

type fixedStats struct{ in, out uint64 }

func (f fixedStats) IsConnected() bool { return true }
func (f fixedStats) Stats() transport.TransportStats {
	return transport.TransportStats{BytesReceived: f.in, BytesSent: f.out}
}

// A classic carrier without a Session reports its traffic too: the app's
// speed counters read it.
func TestIPCStatusWithoutSession(t *testing.T) {
	dir, err := os.MkdirTemp("", "ofipc")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "s.sock")
	srv := ipc.NewServer(path, &coreIPCHandler{})
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	go ipcStatusLoop(srv, fixedStats{in: 1500, out: 700}, nil, nil)

	c, err := ipc.Dial(path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	got := make(chan ipc.StatusPayload, 1)
	c.SetHandler(func(typ ipc.MsgType, body []byte) {
		if typ != ipc.MsgStatus {
			return
		}
		var p ipc.StatusPayload
		if json.Unmarshal(body, &p) == nil {
			select {
			case got <- p:
			default:
			}
		}
	})
	select {
	case p := <-got:
		if !p.Connected || p.BytesIn != 1500 || p.BytesOut != 700 || p.Active != "" {
			t.Fatalf("status = %+v", p)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no status")
	}
}
