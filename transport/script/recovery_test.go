package script

import (
	"testing"
	"time"
)

func TestRecoveryLadderClimbsOneRungPerStreak(t *testing.T) {
	l := NewRecoveryLadder(RecoveryPolicy{RecheckAfter: 2, RollbackAfter: 4, FallbackAfter: 6})
	t0 := time.Unix(0, 0)
	want := []RecoveryAction{
		RecoverNone,     // fail 1
		RecoverRecheck,  // fail 2
		RecoverNone,     // fail 3
		RecoverRollback, // fail 4
		RecoverNone,     // fail 5
		RecoverFallback, // fail 6
		RecoverNone,     // fail 7: nothing left to climb
	}
	for i, w := range want {
		got := l.OnFailed(t0.Add(time.Duration(i) * time.Second))
		if got != w {
			t.Fatalf("failure %d: got %v (%q), want %v (%q)", i+1, got, got.Code(), w, w.Code())
		}
	}
}

func TestRecoveryLadderStableConnectionResets(t *testing.T) {
	p := RecoveryPolicy{RecheckAfter: 2, RollbackAfter: 4, FallbackAfter: 6, StableFor: 60 * time.Second}
	l := NewRecoveryLadder(p)
	now := time.Unix(100, 0)

	// Climb to a recheck.
	l.OnFailed(now)
	now = now.Add(time.Second)
	if a := l.OnFailed(now); a != RecoverRecheck {
		t.Fatalf("expected recheck, got %q", a.Code())
	}
	// Connect and hold it well past StableFor, then drop: the next failure is
	// a brand-new streak and must start from the bottom rung.
	now = now.Add(time.Second)
	l.OnConnected(now)
	now = now.Add(2 * time.Minute)
	if a := l.OnFailed(now); a != RecoverNone {
		t.Fatalf("after a stable run, first failure should be RecoverNone, got %q", a.Code())
	}
	if l.Fails() != 1 {
		t.Fatalf("stable run should have reset the count; fails=%d", l.Fails())
	}
}

func TestRecoveryLadderFlapDoesNotReset(t *testing.T) {
	p := RecoveryPolicy{RecheckAfter: 2, RollbackAfter: 4, FallbackAfter: 6, StableFor: 60 * time.Second}
	l := NewRecoveryLadder(p)
	now := time.Unix(0, 0)

	l.OnFailed(now)                       // 1
	now = now.Add(time.Second)            //
	l.OnConnected(now)                    // connects...
	now = now.Add(5 * time.Second)        // ...but only for 5s (< StableFor)
	a := l.OnFailed(now)                  // 2: the brief connect must NOT have reset
	if a != RecoverRecheck {
		t.Fatalf("a sub-StableFor flap must not reset the ladder; got %q want recheck", a.Code())
	}
}

func TestRecoveryLadderWindowStartsFreshStreak(t *testing.T) {
	p := RecoveryPolicy{RecheckAfter: 2, RollbackAfter: 4, FallbackAfter: 6, Window: 10 * time.Minute}
	l := NewRecoveryLadder(p)
	now := time.Unix(0, 0)

	l.OnFailed(now) // one lonely failure
	// ...then quiet for longer than the window.
	now = now.Add(30 * time.Minute)
	if a := l.OnFailed(now); a != RecoverNone {
		t.Fatalf("a failure after the window should start fresh (RecoverNone), got %q", a.Code())
	}
	if l.Fails() != 1 {
		t.Fatalf("window should have reset the count; fails=%d", l.Fails())
	}
}

func TestRecoveryPolicyNormalizeEnforcesIncreasing(t *testing.T) {
	// A misconfiguration that would make rollback fire before recheck.
	p := RecoveryPolicy{RecheckAfter: 5, RollbackAfter: 3, FallbackAfter: 1}.Normalize()
	if !(p.RecheckAfter < p.RollbackAfter && p.RollbackAfter < p.FallbackAfter) {
		t.Fatalf("thresholds not strictly increasing after Normalize: %+v", p)
	}
}

func TestRecoveryPolicyNormalizeFillsZeros(t *testing.T) {
	p := RecoveryPolicy{}.Normalize()
	d := DefaultRecoveryPolicy()
	if p != d {
		t.Fatalf("empty policy should normalize to the default; got %+v want %+v", p, d)
	}
}

func TestRecoveryActionCode(t *testing.T) {
	cases := map[RecoveryAction]string{
		RecoverNone:     "",
		RecoverRecheck:  "recheck",
		RecoverRollback: "rollback",
		RecoverFallback: "fallback",
	}
	for a, want := range cases {
		if a.Code() != want {
			t.Fatalf("action %d code = %q, want %q", a, a.Code(), want)
		}
	}
}
