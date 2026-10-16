package server

import (
	"context"
	"strings"
	"testing"

	"github.com/p1neappleXpress/OpenFlux/provision"
)

func TestDocFactorySlotDropsUnconfiguredCarriers(t *testing.T) {
	sp := &scriptProvisioner{
		cfg:  &Config{NodeScript: "/n.sh"},
		logf: func(string, ...any) {},
		docs: accountDocFactory{}, // no accounts configured
		rooms: func(context.Context) (string, error) { return "WyJyb29tIl0", nil },
	}
	ts, err := sp.channelTransports(context.Background(), []provision.ChannelTransport{
		{Type: "cupsonline"}, {Type: "vyandex"}, {Type: "mailru"}, {Type: "boards"},
	}, "owner-1")
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, x := range ts {
		types = append(types, x.Type)
	}
	if strings.Join(types, ",") != "cupsonline" {
		t.Fatalf("only cups survives without accounts, got %v", types)
	}
}

func TestDocFactoryMakesDocumentWhenConfigured(t *testing.T) {
	f := &fakeDocFactory{url: "https://docs.yandex.ru/edit/d/abcdefghijklmnopqrstuv", seen: map[string]string{}}
	sp := &scriptProvisioner{
		cfg:  &Config{NodeScript: "/n.sh"},
		logf: func(string, ...any) {},
		docs: f,
		rooms: func(context.Context) (string, error) { return "WyJyb29tIl0", nil },
	}
	ts, err := sp.channelTransports(context.Background(), []provision.ChannelTransport{
		{Type: "cupsonline"}, {Type: "vyandex"},
	}, "owner-1")
	if err != nil {
		t.Fatal(err)
	}
	var got string
	for _, x := range ts {
		if x.Type == "vyandex" {
			got = x.URL
		}
	}
	if got != "https://docs.yandex.ru/edit/d/abcdefghijklmnopqrstuv" {
		t.Fatalf("the factory's document did not reach the transports: %v", ts)
	}
	if f.seen["vyandex"] != "owner-1" {
		t.Fatalf("the owner did not reach the factory: %v", f.seen)
	}
	if _, ok := f.seen["cupsonline"]; ok {
		t.Fatalf("a non-document carrier must not ask the factory: %v", f.seen)
	}
}

type fakeDocFactory struct {
	url  string
	seen map[string]string // carrier -> owner
}

func (f *fakeDocFactory) Make(_ context.Context, carrier, owner string) (string, string, error) {
	f.seen[carrier] = owner
	return f.url, "cookies", nil
}
