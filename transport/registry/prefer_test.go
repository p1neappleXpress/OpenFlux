package registry

import "testing"

func TestPreferParam(t *testing.T) {
	for _, c := range []struct {
		in   interface{}
		want bool
	}{
		{nil, true}, {true, true}, {false, false}, {"false", false}, {"no", false}, {"off", false}, {"0", false},
		{"yes", true}, {"1", true}, {"true", true}, {float64(0), false}, {float64(1), true}, {0, false}, {1, true},
	} {
		params := map[string]interface{}{}
		if c.in != nil {
			params["prefer"] = c.in
		}
		if got := preferParam(params); got != c.want {
			t.Errorf("prefer=%v: got %v, want %v", c.in, got, c.want)
		}
	}
}
