package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"

	"github.com/p1neappleXpress/OpenFlux/share"
)

// roomLister is a transport whose address for clients exists only once it
// runs: cupsonline, whose exit creates its rooms at start when given no --url.
type roomLister interface {
	RoomList() string
	OnRoomList(func(packed string))
}

// shareConfig describes this exit to its clients for --share: the same
// transports, secret and encryption context, with direct pointing at host
// and cupsonline carrying the rooms its running transport (in rooms, by
// spec name) entered. It also returns the transports left out and why.
func shareConfig(specs []transportSpec, session bool, codec, secret, context, host string, rooms map[string]roomLister) (share.Config, []string) {
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
			if r := rooms[s.Name]; r != nil {
				if list := r.RoomList(); list != "" {
					t.URL = list
				}
			}
			if t.URL == "" {
				skipped = append(skipped, s.Name+": no rooms yet; the link is printed again once they exist")
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
	r := share.Make(c)
	if r.Error != "" {
		log.Printf("--share: %s", r.Error)
		return
	}
	link := r.Link
	qr, err := share.Terminal(link)
	if err != nil {
		log.Printf("--share: %v", err)
		return
	}
	log.Printf("Share link for clients (contains the encryption key): %s", link)
	fmt.Fprint(os.Stderr, qr)
	// The bare link on a line of its own, to copy without the log prefix.
	fmt.Fprintln(os.Stderr, link)
}

// runParseLink reads an openflux:// link (arg, or stdin for "-") with the
// core's parser and prints share.Result as JSON: {"config":...,"context":...}
// or {"error":...,"code":...,"param":...}. Apps word the code themselves.
func runParseLink(arg string, stdin io.Reader, stdout io.Writer) int {
	link, err := argOrStdin(arg, stdin)
	if err != nil {
		return writeLinkResult(stdout, share.Failed(err))
	}
	return writeLinkResult(stdout, share.Read(link))
}

// runMakeLink builds the link for a share.Config JSON (arg, or stdin for
// "-") the way every client exports one and prints share.Result as JSON:
// {"link":...,"config":...,"context":...} or the error.
func runMakeLink(arg string, stdin io.Reader, stdout io.Writer) int {
	cfg, err := argOrStdin(arg, stdin)
	if err != nil {
		return writeLinkResult(stdout, share.Failed(err))
	}
	return writeLinkResult(stdout, share.MakeJSON(cfg))
}

func argOrStdin(arg string, stdin io.Reader) (string, error) {
	if arg != "-" {
		return arg, nil
	}
	b, err := io.ReadAll(io.LimitReader(stdin, 64<<10))
	return string(b), err
}

func writeLinkResult(w io.Writer, r share.Result) int {
	fmt.Fprintln(w, r.JSON())
	if r.Error != "" {
		return 1
	}
	return 0
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
