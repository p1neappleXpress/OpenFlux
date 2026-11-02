//go:build !mobile

package main

// --cups-echo: a regression probe for the raw control channel. It starts a
// cups transport on the rooms in --url, sends one tagged line a second, and
// prints everything it receives. No cbir, no session, no encryption: if two
// instances cannot see each other here, the rooms themselves are the problem.

import (
	"fmt"
	"os"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport"
	"github.com/p1neappleXpress/OpenFlux/transport/cupsonline"
)

func runCupsEcho(rooms, tag string) {
	cfg := transport.DefaultConfig()
	// OPENFLUX_ECHO_CLIENT=1 builds the transport the way a phone does, which is
	// the mode the pointer adapter uses on the client side.
	client := os.Getenv("OPENFLUX_ECHO_CLIENT") == "1"
	t := cupsonline.NewCupsonlineTransport(rooms, cfg, client)
	t.Receive(func(msg []byte) {
		fmt.Printf("RECV %q\n", string(msg))
	})
	if err := t.Start(); err != nil {
		fmt.Printf("START FAILED: %v\n", err)
		os.Exit(1)
	}
	go func() {
		i := 0
		for {
			time.Sleep(time.Second)
			i++
			msg := fmt.Sprintf("OFXECHO:%s:%d", tag, i)
			if err := t.Send([]byte(msg)); err != nil {
				fmt.Printf("SEND FAILED: %v\n", err)
				continue
			}
			fmt.Printf("SENT %q\n", msg)
		}
	}()
	select {}
}
