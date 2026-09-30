// Command phpboxfetch dials one destination through a phpbox exit and prints
// the response - a manual end-to-end check of the transport/phpbox client
// against a running deploy/phpbox/phpbox.php.
//
// Local proof (no anti-bot in the way):
//
//	PHPBOX_ALLOW_PRIVATE=1 PHP_CLI_SERVER_WORKERS=6 \
//	    php -S 127.0.0.1:8085 deploy/phpbox/phpbox.php &
//	go run ./deploy/phpbox/cmd/phpboxfetch \
//	    -url http://127.0.0.1:8085/phpbox.php -token CHANGE-ME \
//	    -target https://icanhazip.com/
//
// Against a real host the client must first pass whatever bot check the host
// puts in front of PHP (e.g. InfinityFree serves a JS challenge to non-
// browsers); that is a separate problem from the tunnel itself.
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport/phpbox"
)

func main() {
	endpoint := flag.String("url", "", "phpbox.php URL, e.g. http://127.0.0.1:8085/phpbox.php")
	token := flag.String("token", "CHANGE-ME", "phpbox token (PHPBOX_TOKEN on the exit)")
	target := flag.String("target", "https://icanhazip.com/", "URL to fetch through the exit")
	timeout := flag.Duration("timeout", 20*time.Second, "overall timeout")
	flag.Parse()
	if *endpoint == "" {
		fmt.Fprintln(os.Stderr, "-url is required")
		os.Exit(2)
	}

	u, err := url.Parse(*target)
	if err != nil || u.Host == "" {
		fmt.Fprintln(os.Stderr, "bad -target:", err)
		os.Exit(2)
	}
	host := u.Hostname()
	port := 80
	if u.Scheme == "https" {
		port = 443
	}
	if p := u.Port(); p != "" {
		port, _ = strconv.Atoi(p)
	}

	sess := "fetch" + strconv.FormatInt(time.Now().UnixNano(), 36)
	c := phpbox.NewClient(*endpoint, *token, sess)
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	conn, err := c.Dial(ctx, host, port)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dial through phpbox:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "opened stream to %s:%d through %s\n", host, port, *endpoint)

	var rw io.ReadWriter = conn
	if u.Scheme == "https" {
		tc := tls.Client(conn, &tls.Config{ServerName: host})
		if err := tc.Handshake(); err != nil {
			fmt.Fprintln(os.Stderr, "TLS handshake:", err)
			os.Exit(1)
		}
		rw = tc
	}

	path := u.RequestURI()
	fmt.Fprintf(rw, "GET %s HTTP/1.1\r\nHost: %s\r\nUser-Agent: phpboxfetch\r\nAccept: */*\r\nConnection: close\r\n\r\n", path, host)

	br := bufio.NewReader(rw)
	status, err := br.ReadString('\n')
	if err != nil {
		fmt.Fprintln(os.Stderr, "read status:", err)
		os.Exit(1)
	}
	fmt.Print("HTTP status: ", status)
	// Skip headers, print the body (icanhazip returns just the IP).
	for {
		line, err := br.ReadString('\n')
		if err != nil || line == "\r\n" || line == "\n" {
			break
		}
	}
	body, _ := io.ReadAll(br)
	fmt.Printf("body: %s\n", body)
}
