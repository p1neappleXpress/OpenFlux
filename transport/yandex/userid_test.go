package yandex

import (
	"errors"
	"testing"
	"time"

	"openflux/transport"
)

// A reconnect must not rejoin the document under the previous session's
// participant id: Yandex still holds that participant for a while after a
// drop and refuses the duplicate right after CONNECT.
func TestReconnectUsesFreshUserID(t *testing.T) {
	seen := make(chan string, 1)
	orig := fetchDocInfo
	fetchDocInfo = func(_ *YandexDocsTransport, _, userID string) (YandexDocsInfo, error) {
		select {
		case seen <- userID:
		default:
		}
		return YandexDocsInfo{}, errors.New("test: no document")
	}
	defer func() { fetchDocInfo = orig }()

	tr := NewYandexDocsTransport("https://docs.example/d", transport.DefaultConfig())
	if err := tr.BaseTransport.Start(); err != nil {
		t.Fatal(err)
	}
	defer tr.Stop()
	tr.session = &DocSession{UserID: "stale-participant", WriteQueue: make(chan []byte, 1)}

	tr.connectToDoc(0)
	select {
	case id := <-seen:
		if id == "stale-participant" {
			t.Fatal("reconnect reused the previous participant id")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("connectToDoc never fetched the document")
	}

	if a, b := tr.nextUserID(), tr.nextUserID(); a == b {
		t.Fatalf("nextUserID repeated %q", a)
	}
}
