//go:build !mobile

package main

// runFetch is a diagnostic: one HTTP GET with the same client the transports
// use, printing the status and a slice of the body. It is what a phone needs to
// say whether the relay answers it at all.

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

func runFetch(url string) {
	// The probe runs before the shared resolver setup, so apply OPENFLUX_DNS
	// itself: on Android there is no usable /etc/resolv.conf.
	if dns := os.Getenv("OPENFLUX_DNS"); dns != "" {
		host := dns
		if !strings.Contains(host, ":") {
			host += ":53"
		}
		net.DefaultResolver = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, host)
			},
		}
	}
	c := &http.Client{Timeout: 20 * time.Second}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		fmt.Println("bad url:", err)
		os.Exit(1)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:153.0) Gecko/20100101 Firefox/153.0")
	res, err := c.Do(req)
	if err != nil {
		fmt.Println("FETCH FAILED:", err)
		os.Exit(1)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 400))
	fmt.Printf("status=%d len=%d body=%.160s\n", res.StatusCode, len(body), string(body))
}
