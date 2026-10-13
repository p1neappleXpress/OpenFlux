package mobile

import (
	"encoding/json"
	"os"
	"testing"
)

// The script calls the apps make (gomobile on Android, mobile/ios/script.go on
// iOS) are thin over transport/script; these pin what the apps get back.

// The engine runs a whole open -> connected -> send -> emit round trip in-process.
func TestScriptEngineSelfTestRoundTrip(t *testing.T) {
	var r struct {
		OK     bool   `json:"ok"`
		Engine string `json:"engine"`
		Sent   int    `json:"sent"`
		Recv   int    `json:"recv"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal([]byte(ScriptEngineSelfTest(t.TempDir())), &r); err != nil {
		t.Fatal(err)
	}
	if !r.OK || r.Engine != "goja" || r.Recv < r.Sent || r.Sent == 0 {
		t.Fatalf("self-test failed: %+v", r)
	}
	if !ScriptEngineAvailable() {
		t.Fatal("engine reported unavailable")
	}
}

// A shipped transport inspects as validly signed and official under the official key.
func TestInspectTransportOfficialBundle(t *testing.T) {
	data, err := os.ReadFile("../transport/script/js/cupsonline.js")
	if err != nil {
		t.Fatal(err)
	}
	sig, err := os.ReadFile("../transport/script/js/cupsonline.js.sig")
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		Signature string `json:"signature"`
		Official  bool   `json:"official"`
		Name      string `json:"name"`
	}
	if err := json.Unmarshal([]byte(InspectTransport(data, sig, OfficialScriptKey())), &r); err != nil {
		t.Fatal(err)
	}
	if r.Signature != "valid" || !r.Official || r.Name == "" {
		t.Fatalf("official bundle inspected as %+v", r)
	}
	// A wrong signature never passes.
	if err := json.Unmarshal([]byte(InspectTransport(data, []byte("not a signature"), OfficialScriptKey())), &r); err != nil {
		t.Fatal(err)
	}
	if r.Signature == "valid" {
		t.Fatal("a bad signature inspected as valid")
	}
}

func TestScriptKeyAndVersions(t *testing.T) {
	if len(OfficialScriptKey()) != 64 {
		t.Fatalf("official key %q is not 32 bytes of hex", OfficialScriptKey())
	}
	if fp := ScriptFingerprint(OfficialScriptKey()); len(fp) != 64 {
		t.Fatalf("fingerprint %q", fp)
	}
	if ScriptFingerprint("zz") != "" {
		t.Fatal("an undecodable key got a fingerprint")
	}
	if CompareScriptVersions("1.1.0", "1.0.9") != 1 || CompareScriptVersions("1.0.0", "1.0.0") != 0 ||
		CompareScriptVersions("1.0.0", "1.2.0") != -1 {
		t.Fatal("version order")
	}
}

// The update calls answer a malformed call with the usage code instead of reaching the network.
func TestScriptUpdateCallsRejectUsage(t *testing.T) {
	for name, got := range map[string]string{
		"check, no id":    CheckScriptUpdate(`{}`, "stable"),
		"check, not json": CheckScriptUpdate(`nope`, "stable"),
		"apply, no dir":   ApplyScriptUpdate(`{"id":"x","pubkey":"ab"}`, "stable", "", false),
		"rollback, no id": RollbackScript(`{}`, t.TempDir()),
	} {
		if got != usageReport {
			t.Errorf("%s: %s", name, got)
		}
	}
}
