package yandex

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// Strict Yandex balancers only accept a websocket that joins an engine.io v4
// session opened by long polling: GET ?transport=polling for a sid, dial the
// websocket with &sid=..., then upgrade it with 2probe / 3probe / 5 before
// any socket.io frame. A bare websocket is closed right after CONNECT with
// close 1005 (#31). Lenient balancers accept both.

// engineIOPoll opens an engine.io session by long polling and returns its
// sid and the cookies to dial the websocket with: the document's own plus
// whatever the polling response set.
func engineIOPoll(info YandexDocsInfo) (sid, cookies string, err error) {
	pollURL := strings.Replace(info.WsURL, "transport=websocket", "transport=polling", 1)
	pollURL = strings.Replace(pollURL, "wss://", "https://", 1)
	pollURL = strings.Replace(pollURL, "ws://", "http://", 1)

	req, err := http.NewRequest("GET", pollURL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Origin", info.Origin)
	if info.CookieStr != "" {
		req.Header.Set("Cookie", info.CookieStr)
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("polling status %d", resp.StatusCode)
	}

	// The body is an engine.io OPEN packet, "0{...}", possibly behind a
	// payload length prefix.
	s := string(body)
	i := strings.IndexByte(s, '{')
	if i < 0 {
		return "", "", fmt.Errorf("polling: no open packet")
	}
	var open struct {
		Sid string `json:"sid"`
	}
	if err := json.NewDecoder(strings.NewReader(s[i:])).Decode(&open); err != nil {
		return "", "", fmt.Errorf("polling open packet: %w", err)
	}
	if open.Sid == "" {
		return "", "", fmt.Errorf("polling open packet has no sid")
	}

	cookies = info.CookieStr
	for _, c := range resp.Cookies() {
		if cookies != "" {
			cookies += "; "
		}
		cookies += c.Name + "=" + c.Value
	}
	return open.Sid, cookies, nil
}

// engineIOProbe upgrades a websocket dialed with a polling sid: it sends
// 2probe, waits for 3probe and confirms with 5.
func engineIOProbe(conn *websocket.Conn, timeout time.Duration) error {
	if err := conn.WriteMessage(websocket.TextMessage, []byte("2probe")); err != nil {
		return err
	}
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	defer conn.SetReadDeadline(time.Time{})
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("waiting for 3probe: %w", err)
		}
		if string(msg) == "3probe" {
			break
		}
	}
	return conn.WriteMessage(websocket.TextMessage, []byte("5"))
}

// engineIOAwaitOpen waits for the OPEN packet ("0{...}") a websocket-only
// engine.io session starts with. Writing socket.io frames before it races
// the server's handshake and gets the connection closed (#54).
func engineIOAwaitOpen(conn *websocket.Conn, timeout time.Duration) error {
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	defer conn.SetReadDeadline(time.Time{})
	_, msg, err := conn.ReadMessage()
	if err != nil {
		return fmt.Errorf("waiting for engine.io open: %w", err)
	}
	if len(msg) == 0 || msg[0] != '0' {
		return fmt.Errorf("unexpected first packet %q", shortStr(string(msg), 40))
	}
	return nil
}
