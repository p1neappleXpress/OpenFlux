package server

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/p1neappleXpress/OpenFlux/provision"
)

const (
	cupsList  = "WyJyb29tIl0"
	avitoList = "WyJhIiwiYiJd"
)

func kinds(ts []provision.ChannelTransport) string {
	var out []string
	for _, x := range ts {
		out = append(out, x.Type)
	}
	return strings.Join(out, ",")
}

// A trial pool lists no transports: its channels get what anyone can make
// without signing in, cups.online and Avito Code rooms.
func TestAnEmptyPoolGetsCupsAndAvito(t *testing.T) {
	sp := &scriptProvisioner{
		logf:       func(string, ...any) {},
		rooms:      func(context.Context) (string, error) { return cupsList, nil },
		avitoRooms: func(context.Context) (string, error) { return avitoList, nil },
	}
	ts, err := sp.channelTransports(context.Background(), nil, "owner-1")
	if err != nil {
		t.Fatal(err)
	}
	if kinds(ts) != "avito,cupsonline" || ts[0].URL != avitoList || ts[1].URL != cupsList {
		t.Fatalf("got %+v", ts)
	}
	if !strings.Contains(transportLines(ts), "avito="+avitoList+"\n") {
		t.Fatalf("the installer's config: %q", transportLines(ts))
	}
}

// Avito down: the channel is still made, with cups.online.
func TestAvitoDownLeavesCups(t *testing.T) {
	var logged []string
	sp := &scriptProvisioner{
		logf:       func(f string, a ...any) { logged = append(logged, f) },
		rooms:      func(context.Context) (string, error) { return cupsList, nil },
		avitoRooms: func(context.Context) (string, error) { return "", errors.New("timeout") },
	}
	ts, err := sp.channelTransports(context.Background(), nil, "owner-1")
	if err != nil || kinds(ts) != "cupsonline" {
		t.Fatalf("got %+v %v", ts, err)
	}
	if len(logged) == 0 {
		t.Fatal("the Avito failure is not logged")
	}
	// Listed in the pool, Avito without rooms is made too, and skipped the same way.
	ts, err = sp.channelTransports(context.Background(), []provision.ChannelTransport{{Type: "avito"}, {Type: "cupsonline"}}, "owner-1")
	if err != nil || kinds(ts) != "cupsonline" {
		t.Fatalf("listed: %+v %v", ts, err)
	}
	sp.avitoRooms = func(context.Context) (string, error) { return avitoList, nil }
	ts, err = sp.channelTransports(context.Background(), []provision.ChannelTransport{{Type: "avito"}}, "owner-1")
	if err != nil || kinds(ts) != "avito" || ts[0].URL != avitoList {
		t.Fatalf("avito alone: %+v %v", ts, err)
	}
}

// The config takes Avito without rooms only where the service makes channels.
func TestConfigTakesSelfMadeAvito(t *testing.T) {
	if err := checkAutoTransports([]provision.ChannelTransport{{Type: "avito"}, {Type: "cupsonline"}}); err != nil {
		t.Fatal(err)
	}
	if !hasEmptyCups([]provision.ChannelTransport{{Type: "avito"}}) {
		t.Fatal("an Avito pool without rooms needs autoNode like cups.online")
	}
}
