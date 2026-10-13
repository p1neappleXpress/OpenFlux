package mailru

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/p1neappleXpress/OpenFlux/transport"
)

// chatPeer is a raw, minimal Mail.ru doc participant used to probe the chat
// ("message") channel as a data carrier: it completes the real handshake+auth,
// reads frames (answering pings) and can send a chat message.
type chatPeer struct {
	conn    *websocket.Conn
	session *DocSession
	info    MailruDocsInfo
	mu      sync.Mutex
	recv    []string
}

func (p *chatPeer) got(sub string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range p.recv {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func (p *chatPeer) readLoop() {
	defer func() { _ = recover() }() // closing the conn mid-read is expected at teardown
	for {
		_, msg, err := p.conn.ReadMessage()
		if err != nil {
			return
		}
		text := string(msg)
		if text == "2" {
			_ = p.session.safeWrite(websocket.TextMessage, []byte("3"))
			continue
		}
		p.mu.Lock()
		p.recv = append(p.recv, text)
		p.mu.Unlock()
	}
}

func (p *chatPeer) sendChat(text string) error {
	msg, _ := json.Marshal([]interface{}{"message", map[string]interface{}{"type": "message", "message": text}})
	return p.session.safeWrite(websocket.TextMessage, []byte("42"+string(msg)))
}

func (p *chatPeer) sendRawFrame(frame string) error {
	return p.session.safeWrite(websocket.TextMessage, []byte(frame))
}

func (p *chatPeer) snapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.recv))
	copy(out, p.recv)
	return out
}

func openChatPeer(t *testing.T, wl, username string) *chatPeer {
	t.Helper()
	tr := NewMailruDocsTransport("https://cloud.mail.ru/public/"+wl, transport.DefaultConfig())
	info, err := tr.fetchDocInfo(wl)
	if err != nil {
		t.Fatalf("fetchDocInfo: %v", err)
	}
	dialer := websocket.Dialer{EnableCompression: true, HandshakeTimeout: 15 * time.Second}
	h := http.Header{}
	h.Set("User-Agent", mailruUserAgent)
	h.Set("Accept-Language", "en-US,en;q=0.9")
	h.Set("Origin", "https://docs.datacloudmail.ru")
	conn, _, err := dialer.Dial(info.WsURL, h)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	session := &DocSession{Conn: conn, ready: make(chan struct{}), UserID: username}
	if err := waitMailruSocketIO(session, info.Token); err != nil {
		conn.Close()
		t.Fatalf("handshake: %v", err)
	}
	auth := map[string]interface{}{
		"type": "auth", "docid": info.DocKey, "documentCallbackUrl": info.CallbackURL,
		"token": "fghhfgsjdgfjs",
		"user":  map[string]interface{}{"id": info.EditorUserID, "username": username, "indexUser": -1},
		"editorType": 0, "lastOtherSaveTime": -1, "block": []interface{}{},
		"documentFormatSave": 65, "view": false, "isCloseCoAuthoring": false,
		"openCmd": map[string]interface{}{
			"c": "open", "id": info.DocKey, "userid": info.EditorUserID, "format": info.FileType,
			"url": info.DocURL, "title": info.DocTitle, "lcid": 25, "nobase64": true,
			"convertToOrigin": ".pdf.xps.oxps.djvu",
		},
		"lang": "ru", "mode": "edit", "permissions": info.Permissions,
		"IsAnonymousUser": false, "timezoneOffset": -180, "coEditingMode": "fast",
		"jwtOpen": info.Token, "time": 1000, "supportAuthChangesAck": true,
	}
	part, _ := json.Marshal([]interface{}{"message", auth})
	_ = session.safeWrite(websocket.TextMessage, []byte("42"+string(part)))
	p := &chatPeer{conn: conn, session: session, info: info}
	go p.readLoop()
	return p
}

// TestLiveMailruChatRelay opens two participants on the same doc and checks that
// a chat ("message") sent by A is relayed by the server to B, at increasing
// payload sizes. This probes the chat channel as the transport's new data carrier.
//
//	OPENFLUX_MAILRU_DUMP=DaGR/ipfU8AjqE go test -run TestLiveMailruChatRelay -v ./transport/mailru/
func TestLiveMailruChatRelay(t *testing.T) {
	wl := os.Getenv("OPENFLUX_MAILRU_DUMP")
	if wl == "" {
		t.Skip("set OPENFLUX_MAILRU_DUMP=<bare weblink>")
	}
	a := openChatPeer(t, wl, "peerA")
	defer a.conn.Close()
	b := openChatPeer(t, wl, "peerB")
	defer b.conn.Close()

	// let both finish auth / join
	time.Sleep(4 * time.Second)

	sizes := []int{16, 256, 1024, 8192, 65536, 262144}
	for _, n := range sizes {
		marker := fmt.Sprintf("OFXz%dz", n)
		payload := marker + strings.Repeat("A", n)
		if err := a.sendChat(payload); err != nil {
			t.Logf("size %7d: A.sendChat error: %v", n, err)
			continue
		}
		ok := false
		deadline := time.Now().Add(4 * time.Second)
		for time.Now().Before(deadline) {
			if b.got(marker) {
				ok = true
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		aAlive := !a.got("disconnectReason")
		t.Logf("size %7d bytes: relayed_to_B=%v  A_still_ok=%v", n, ok, aAlive)
		if a.got("disconnectReason") {
			t.Logf("  A received disconnectReason after size %d — server rejected this chat", n)
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
}
