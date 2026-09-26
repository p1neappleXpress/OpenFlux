package main

import (
	"reflect"
	"testing"
)

func TestExpandDebugFlags(t *testing.T) {
	cases := []struct {
		in   []string
		want []string
	}{
		{[]string{"openflux", "-d"}, []string{"openflux", "--debug=1"}},
		{[]string{"openflux", "-dd"}, []string{"openflux", "--debug=2"}},
		{[]string{"openflux", "-ddd"}, []string{"openflux", "--debug=3"}},
		{[]string{"openflux", "-dddd"}, []string{"openflux", "--debug=3"}},
		{[]string{"openflux", "-d=2"}, []string{"openflux", "--debug=2"}},
		{[]string{"openflux", "--debug=2"}, []string{"openflux", "--debug=2"}},
		{[]string{"openflux", "--debug", "3"}, []string{"openflux", "--debug", "3"}},
		{[]string{"openflux", "--debug", "-r=exit"}, []string{"openflux", "--debug=1", "--role=exit"}},
		{[]string{"openflux", "--debug"}, []string{"openflux", "--debug=1"}},
		// Single-dash long flags that start with d are not debug flags.
		{[]string{"openflux", "-direct-listen=:9000"}, []string{"openflux", "-direct-listen=:9000"}},
		{[]string{"openflux", "-direct-dial", "1.2.3.4:9000"}, []string{"openflux", "-direct-dial", "1.2.3.4:9000"}},
	}
	for _, c := range cases {
		if got := expandShortFlags(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("expandShortFlags(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
