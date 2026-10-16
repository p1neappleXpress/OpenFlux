package mailru

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/p1neappleXpress/OpenFlux/transport"
)

// TestLiveMailruDump connects to a real public Mail.ru document, completes the
// Engine.IO/Socket.IO handshake and the editor auth, then listens WITHOUT
// sending any editor write and logs every server frame verbatim. It exists to
// see exactly what the server sends after auth (does it send authChanges? an
// init? nothing?) and what it does the moment we do send a write. Diagnostic
// only; drives the real service, so it is env-gated:
//
//	OPENFLUX_MAILRU_DUMP=DaGR/ipfU8AjqE \
//	  [OPENFLUX_MAILRU_DUMP_DUR=30s] \
//	  [OPENFLUX_MAILRU_DUMP_WRITE_AFTER=12s] \   # send ONE cursor write then watch
//	  [OPENFLUX_MAILRU_DUMP_ACKFLAG=false] \     # auth's supportAuthChangesAck (default true)
//	  ./mailru.test -test.run '^TestLiveMailruDump$' -test.v -test.timeout 120s
func TestLiveMailruDump(t *testing.T) {
	wl := os.Getenv("OPENFLUX_MAILRU_DUMP")
	if wl == "" {
		t.Skip("set OPENFLUX_MAILRU_DUMP=<bare weblink, e.g. DaGR/ipfU8AjqE>")
	}
	dur := 30 * time.Second
	if v := os.Getenv("OPENFLUX_MAILRU_DUMP_DUR"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			dur = d
		}
	}
	var writeAfter time.Duration
	if v := os.Getenv("OPENFLUX_MAILRU_DUMP_WRITE_AFTER"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			writeAfter = d
		}
	}
	ackFlag := os.Getenv("OPENFLUX_MAILRU_DUMP_ACKFLAG") != "false"
	anonFlag := os.Getenv("OPENFLUX_MAILRU_DUMP_ANON") == "true"
	openBin := os.Getenv("OPENFLUX_MAILRU_DUMP_OPENBIN") == "true"
	preamble := os.Getenv("OPENFLUX_MAILRU_DUMP_PREAMBLE") == "true"

	tr := NewMailruDocsTransport("https://cloud.mail.ru/public/"+wl, transport.DefaultConfig())
	info, err := tr.fetchDocInfo(wl)
	if err != nil {
		t.Fatalf("fetchDocInfo: %v", err)
	}
	t.Logf("docKey=%s fileType=%s title=%q perms=%v", info.DocKey, info.FileType, info.DocTitle, info.Permissions)
	t.Logf("info.Token(JWT) = %s", info.Token)
	if p := decodeJWTPayload(info.Token); p != "" {
		t.Logf("info.Token payload = %s", p)
	}

	dialer := websocket.Dialer{EnableCompression: true, HandshakeTimeout: 15 * time.Second}
	headers := http.Header{}
	headers.Set("User-Agent", mailruUserAgent)
	headers.Set("Accept-Language", "en-US,en;q=0.9")
	headers.Set("Origin", "https://docs.datacloudmail.ru")

	conn, _, err := dialer.Dial(info.WsURL, headers)
	if err != nil {
		t.Fatalf("dial %s: %v", info.WsURL, err)
	}
	defer conn.Close()
	session := &DocSession{Conn: conn, ready: make(chan struct{}), UserID: "dump"}

	if err := waitMailruSocketIO(session, info.Token); err != nil {
		t.Fatalf("socket.io handshake: %v", err)
	}

	authMsg := map[string]interface{}{
		"type": "auth", "docid": info.DocKey, "documentCallbackUrl": info.CallbackURL,
		"token": "fghhfgsjdgfjs",
		"user":  map[string]interface{}{"id": info.EditorUserID, "username": "dump", "indexUser": -1},
		"editorType": 0, "lastOtherSaveTime": -1, "block": []interface{}{},
		"documentFormatSave": 65, "view": false, "isCloseCoAuthoring": false,
		"openCmd": map[string]interface{}{
			"c": "open", "id": info.DocKey, "userid": info.EditorUserID, "format": info.FileType,
			"url": info.DocURL, "title": info.DocTitle, "lcid": 25, "nobase64": true,
			"convertToOrigin": ".pdf.xps.oxps.djvu",
		},
		"lang": "ru", "mode": "edit", "permissions": info.Permissions,
		"IsAnonymousUser": anonFlag, "timezoneOffset": -180, "coEditingMode": "fast",
		"jwtOpen": info.Token, "time": 1000, "supportAuthChangesAck": ackFlag,
	}
	part, _ := json.Marshal([]interface{}{"message", authMsg})
	if err := session.safeWrite(websocket.TextMessage, []byte("42"+string(part))); err != nil {
		t.Fatalf("send auth: %v", err)
	}
	start := time.Now()
	ms := func() int64 { return time.Since(start).Milliseconds() }
	t.Logf("+%6dms >>> auth sent (supportAuthChangesAck=%v IsAnonymousUser=%v openBin=%v preamble=%v)", ms(), ackFlag, anonFlag, openBin, preamble)

	// The real editor's post-auth, pre-edit preamble, captured from a live
	// anonymous session. Theory: the 2026.2.1.2268 server accepts edit writes
	// only after the client reports the document is open/ready (and pulls
	// messages). We skip it today and get "access deny" on the first write.
	sendPreamble := func() {
		for _, f := range []string{
			`42["message",{"type":"clientLog","level":"debug","msg":"onDownloadFile time:120"}]`,
			`42["message",{"type":"clientLog","level":"debug","msg":"onOpenDocument time:70"}]`,
			`42["message",{"type":"clientLog","level":"debug","msg":"onLoadFonts time:900"}]`,
			`42["message",{"type":"getMessages"}]`,
			`42["message",{"type":"clientLog","level":"debug","msg":"onDocumentContentReady time:5000 memory:{\"totalJSHeapSize\":145386919,\"usedJSHeapSize\":129760103,\"jsHeapSizeLimit\":4395630592}"}]`,
			`42["message",{"type":"message"}]`,
		} {
			if err := session.safeWrite(websocket.TextMessage, []byte(f)); err != nil {
				t.Logf("+%6dms >>> PREAMBLE write failed: %v", ms(), err)
				return
			}
			time.Sleep(60 * time.Millisecond)
		}
		t.Logf("+%6dms >>> PREAMBLE sent (6 frames)", ms())
	}

	// Fire the test write from a timer so it is not gated behind the next
	// inbound frame (the server can be silent for ~25s between pings).
	cursorVal := "18;AAAAAA=="
	if v := os.Getenv("OPENFLUX_MAILRU_DUMP_CURSOR"); v != "" {
		cursorVal = v
	}
	if writeAfter > 0 {
		time.AfterFunc(writeAfter, func() {
			w := fmt.Sprintf(`42["message",{"type":"cursor","cursor":%q}]`, cursorVal)
			if err := session.safeWrite(websocket.TextMessage, []byte(w)); err != nil {
				t.Logf("+%6dms >>> TEST WRITE failed: %v", ms(), err)
			} else {
				t.Logf("+%6dms >>> TEST WRITE %s", ms(), w)
			}
		})
	}

	_ = conn.SetReadDeadline(time.Now().Add(dur + 5*time.Second))
	sentPreamble := false
	for time.Since(start) < dur {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			t.Logf("+%6dms <<< READ ERROR: %v", ms(), err)
			break
		}
		full := string(msg)
		if full == "2" { // Engine.IO ping -> pong, the one write we must always do
			_ = session.safeWrite(websocket.TextMessage, []byte("3"))
			t.Logf("+%6dms <<< 2 (ping) -> 3", ms())
			continue
		}
		text := full
		if len(text) > 500 {
			text = text[:500] + fmt.Sprintf("…(+%d bytes)", len(msg)-500)
		}
		t.Logf("+%6dms <<< %s", ms(), text)

		// On documentOpen, optionally replay the real editor's preamble before
		// any edit write, to test whether it unlocks edits.
		if preamble && !sentPreamble && strings.Contains(full, `"documentOpen"`) {
			sentPreamble = true
			go sendPreamble()
		}

		// Optionally mimic the real editor "opening" the document by fetching
		// its Editor.bin, in case write access is gated on having opened it.
		if openBin && strings.Contains(full, `"Editor.bin"`) {
			if u := extractEditorBinURL(full); u != "" {
				go func(u string) {
					req, _ := http.NewRequest("GET", u, nil)
					req.Header.Set("User-Agent", mailruUserAgent)
					req.Header.Set("Referer", "https://docs.datacloudmail.ru/")
					resp, err := http.DefaultClient.Do(req)
					if err != nil {
						t.Logf("+%6dms Editor.bin GET error: %v", ms(), err)
						return
					}
					defer resp.Body.Close()
					n, _ := io.Copy(io.Discard, resp.Body)
					t.Logf("+%6dms Editor.bin GET -> HTTP %d, %d bytes", ms(), resp.StatusCode, n)
				}(u)
			}
		}
	}
	t.Logf("+%6dms done (dur=%v writeAfter=%v)", ms(), dur, writeAfter)
}

// decodeJWTPayload base64url-decodes the claims (middle) segment of a JWT so we
// can see what permissions the server actually granted this session.
func decodeJWTPayload(tok string) string {
	parts := strings.Split(tok, ".")
	if len(parts) < 2 {
		return ""
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Some encoders pad; try the padded variant too.
		if b, err = base64.URLEncoding.DecodeString(parts[1]); err != nil {
			return ""
		}
	}
	return string(b)
}

// extractEditorBinURL pulls the "Editor.bin":"<url>" value out of a documentOpen frame.
func extractEditorBinURL(s string) string {
	const key = `"Editor.bin":"`
	i := strings.Index(s, key)
	if i < 0 {
		return ""
	}
	rest := s[i+len(key):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	return rest[:j]
}
