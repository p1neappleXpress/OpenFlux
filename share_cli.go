package main

import (
	"fmt"
	"log"
	"net"
	"os"

	"openflux/share"
)

// shareConfig describes this exit to its clients for --share: the same
// transports, secret and encryption context, with direct pointing at host.
// It also returns the transports left out and why.
func shareConfig(specs []transportSpec, session bool, codec, secret, context, host string) (share.Config, []string) {
	name := "OpenFlux"
	if h, err := os.Hostname(); err == nil && h != "" {
		name += " " + h
	}
	c := share.Config{Name: name, Negotiate: session, Secret: secret, Context: context}
	if codec != codecBatched {
		c.Codec = codec
	}
	var skipped []string
	for _, s := range specs {
		t := share.Transport{Type: s.Type, Priority: s.Priority, URL: s.URL}
		if session && s.Name != s.Type {
			t.Name = s.Name
		}
		switch s.Type {
		case "direct":
			listen, _ := s.Params["listen"].(string)
			_, port, err := net.SplitHostPort(listen)
			if err != nil || host == "" {
				skipped = append(skipped, s.Name+": no address clients could dial (set --share-host)")
				continue
			}
			t.Dial = net.JoinHostPort(host, port)
		case "oneme":
			skipped = append(skipped, s.Name+": a MAX token belongs to one account; clients need their own")
			continue
		case "cupsonline":
			if s.URL == "" {
				skipped = append(skipped, s.Name+": rooms are created at startup; share the room list it prints")
				continue
			}
		}
		c.Transports = append(c.Transports, t)
	}
	return c, skipped
}

func printShare(c share.Config, skipped []string) {
	for _, why := range skipped {
		log.Printf("--share: left out %s", why)
	}
	link, err := share.Encode(c)
	if err != nil {
		log.Printf("--share: %v", err)
		return
	}
	qr, err := share.Terminal(link)
	if err != nil {
		log.Printf("--share: %v", err)
		return
	}
	log.Printf("Share link for clients (contains the encryption key): %s", link)
	fmt.Fprint(os.Stderr, qr)
}

// publicIPv4 guesses the address clients should dial: the first global
// unicast IPv4 of this host, preferring a public one to a private one.
func publicIPv4() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	private := ""
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipnet.IP.To4()
		if ip == nil || !ip.IsGlobalUnicast() {
			continue
		}
		if !ip.IsPrivate() {
			return ip.String()
		}
		if private == "" {
			private = ip.String()
		}
	}
	return private
}
