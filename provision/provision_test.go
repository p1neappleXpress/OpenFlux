package provision

import (
	"os"
	"regexp"
	"testing"
)

func TestPinnedScriptHash(t *testing.T) {
	body, err := os.ReadFile("../deploy/node-install.sh")
	if err != nil {
		t.Fatal(err)
	}
	if got := ScriptHash(body); got != PinnedSHA256 {
		t.Fatalf("deploy/node-install.sh changed: sha256 %s, pinned %s; commit it and update pin.go", got, PinnedSHA256)
	}
}

func TestNewChannelIDMatchesScript(t *testing.T) {
	re := regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id, err := NewChannelID()
		if err != nil {
			t.Fatal(err)
		}
		if !re.MatchString(id) {
			t.Fatalf("bad id %q", id)
		}
		seen[id] = true
	}
	if len(seen) < 190 {
		t.Fatalf("ids repeat too often: %d unique of 200", len(seen))
	}
}

func TestNewKey(t *testing.T) {
	k, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(k) {
		t.Fatalf("bad key %q", k)
	}
}

func TestLastJSON(t *testing.T) {
	var v struct {
		OK bool `json:"ok"`
	}
	if err := lastJSON([]byte("noise\n{\"ok\":true}\n\n"), &v); err != nil || !v.OK {
		t.Fatalf("lastJSON: %v %v", err, v)
	}
	if err := lastJSON([]byte("no json"), &v); err == nil {
		t.Fatal("want error")
	}
}

func TestSudoRefusal(t *testing.T) {
	if !isSudoRefusal([]byte("Sorry, try again.\nsudo: 1 incorrect password attempt")) {
		t.Fatal("want refusal")
	}
	if isSudoRefusal([]byte("sh: 1: foo: not found")) {
		t.Fatal("unexpected refusal")
	}
}
