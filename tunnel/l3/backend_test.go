package l3

import "testing"

// Before this fix, --local-ip (tunnel.SetLocalIP in the old, dead location)
// had zero callers anywhere in the repo, and the L3 raw backend always
// auto-detected its own egress IP via a UDP dial to 8.8.8.8, silently
// ignoring any operator override. That meant the iptables RST-drop rule an
// operator sets up around --local-ip could target the wrong address on a
// multi-homed/NAT'd host. resolveEgressIP is the fixed decision point:
// SetLocalIP now actually reaches the value the backend uses.
func TestResolveEgressIPUsesOverrideWhenSet(t *testing.T) {
	t.Cleanup(func() { SetLocalIP("") })

	SetLocalIP("203.0.113.5")
	detectCalled := false
	detect := func() ([4]byte, error) {
		detectCalled = true
		return [4]byte{9, 9, 9, 9}, nil
	}

	got, err := resolveEgressIP(detect)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if detectCalled {
		t.Fatal("resolveEgressIP called the auto-detect function despite an override being set")
	}
	want := [4]byte{203, 0, 113, 5}
	if got != want {
		t.Fatalf("resolveEgressIP() = %v, want %v", got, want)
	}
}

func TestResolveEgressIPFallsBackToDetectWhenUnset(t *testing.T) {
	t.Cleanup(func() { SetLocalIP("") })
	SetLocalIP("")

	want := [4]byte{192, 0, 2, 1}
	got, err := resolveEgressIP(func() ([4]byte, error) { return want, nil })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != want {
		t.Fatalf("resolveEgressIP() = %v, want %v", got, want)
	}
}

func TestResolveEgressIPRejectsInvalidOverride(t *testing.T) {
	t.Cleanup(func() { SetLocalIP("") })
	SetLocalIP("not-an-ip")

	if _, err := resolveEgressIP(func() ([4]byte, error) { return [4]byte{}, nil }); err == nil {
		t.Fatal("expected an error for an invalid --local-ip value, got nil")
	}
}
