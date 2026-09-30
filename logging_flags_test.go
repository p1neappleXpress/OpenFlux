package main

import (
	"github.com/p1neappleXpress/OpenFlux/transport"
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

func TestPickSessionContext(t *testing.T) {
	specs := []transportSpec{
		{Name: "direct", Type: "direct", Priority: 100},
		{Name: "vyandex", Type: "vyandex", Priority: 30, URL: "https://volga/doc"},
		{Name: "yandex", Type: "yandex", Priority: 50, URL: "https://docs/doc"},
	}
	cases := []struct {
		name, explicit, url string
		specs               []transportSpec
		want                string
	}{
		{"explicit wins", "ctx", "https://u", specs, "ctx"},
		{"--url wins over transports", "", "https://u", specs, "https://u"},
		{"highest-priority transport URL", "", "http://#", specs, "https://docs/doc"},
		{"no URL anywhere keeps the old default", "", "http://#", specs[:1], "http://#"},
		{"legacy single transport", "", "http://#", []transportSpec{{Type: "yandex", Priority: 100, URL: "http://#"}}, "http://#"},
		{"cupsonline rooms are not a context", "", "http://#", []transportSpec{
			{Type: "cupsonline", Priority: 100, URL: "WyIxYzE0NGQwZS1lMDQw"},
			{Type: "yandex", Priority: 50, URL: "https://docs/doc"},
		}, "https://docs/doc"},
		{"cupsonline alone", "", "http://#", []transportSpec{{Type: "cupsonline", Priority: 100, URL: "WyIxYzE0NGQwZS1lMDQw"}}, "http://#"},
		// A classic cupsonline client is given the rooms as --url; the exit
		// that created them ran without one. Both must land on the same
		// context (they used to differ, and nothing got through).
		{"classic cupsonline client with rooms as --url", "", "WyIxYzE0NGQwZS1lMDQw",
			[]transportSpec{{Type: "cupsonline", Priority: 100, URL: "WyIxYzE0NGQwZS1lMDQw"}}, "http://#"},
		{"direct host:port is not a context", "", "http://#", []transportSpec{
			{Type: "direct", Priority: 100, URL: "203.0.113.7:9443"},
		}, "http://#"},
	}
	for _, c := range cases {
		if got, _ := transport.KDFContexts(c.explicit, c.url, contextSources(c.specs)); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
