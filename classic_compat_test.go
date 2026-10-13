package main

import "testing"

func TestClassicCompatible(t *testing.T) {
	cases := []struct {
		name                           string
		role                           string
		configured, negotiate, withKey bool
		carriers                       int
		want                           bool
	}{
		{"classic client with a key", roleClient, false, false, true, 1, true},
		{"classic exit with a key", roleExit, false, false, true, 1, true},
		{"classic without a key", roleClient, false, false, false, 1, false},
		{"single-carrier session client", roleClient, true, false, true, 1, true},
		{"multi-carrier session client", roleClient, true, false, true, 3, false},
		{"wizard node (.conf exit, several carriers)", roleExit, true, false, true, 3, true},
		{"single-carrier session exit", roleExit, true, false, true, 1, true},
		{"negotiate client", roleClient, true, true, true, 1, false},
		{"negotiate exit", roleExit, true, true, true, 3, false},
		{"bench sink", roleBenchSink, false, false, true, 1, false},
	}
	for _, c := range cases {
		if got := classicCompatible(c.role, c.configured, c.negotiate, c.withKey, c.carriers); got != c.want {
			t.Errorf("%s: classicCompatible = %v, want %v", c.name, got, c.want)
		}
	}
}
