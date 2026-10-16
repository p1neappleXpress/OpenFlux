package mailru

import (
	"encoding/base64"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport"
)

func cursorEntry(payload string) string {
	return fmt.Sprintf(`{"cursor":"18;%s","time":1790800029352,"user":"anonX","useridoriginal":"anonX"}`, payload)
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// The server batches cursor entries into one message under load. Every data
// entry must come out, in order, and keep-alives anywhere in the batch must
// not cost the data around them.
func TestCursorPayloadsBatch(t *testing.T) {
	ka := cursorEntry("---KA---")
	cases := []struct {
		name string
		text string
		want []string
	}{
		{"one entry", `42["message",{"type":"cursor","messages":[` + cursorEntry(b64("one")) + `]}]`, []string{b64("one")}},
		{"batched data", `42["message",{"type":"cursor","messages":[` + cursorEntry(b64("a")) + "," + cursorEntry(b64("b")) + "," + cursorEntry(b64("c")) + `]}]`, []string{b64("a"), b64("b"), b64("c")}},
		{"keep-alive first, data behind it", `42["message",{"type":"cursor","messages":[` + ka + "," + cursorEntry(b64("data")) + `]}]`, []string{b64("data")}},
		{"data, keep-alive, data", `42["message",{"type":"cursor","messages":[` + cursorEntry(b64("x")) + "," + ka + "," + cursorEntry(b64("y")) + `]}]`, []string{b64("x"), b64("y")}},
		{"only a keep-alive", `42["message",{"type":"cursor","messages":[` + ka + `]}]`, nil},
	}
	for _, c := range cases {
		if got := cursorPayloads(c.text); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// A scheduled reconnect that is already waiting makes a second one a no-op.
func TestSecondReconnectWhileOneIsWaitingReturnsAtOnce(t *testing.T) {
	tr := NewMailruDocsTransport("AbCdEfGh1/IjKlMnOp2", transport.DefaultConfig())
	if err := tr.BaseTransport.Start(); err != nil {
		t.Fatal(err)
	}
	defer tr.BaseTransport.Stop()
	tr.reconnecting.Store(true)
	done := make(chan struct{})
	go func() { tr.scheduleReconnect(0); close(done) }()
	select {
	case <-done:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("a second scheduled reconnect waited out its own backoff instead of yielding")
	}
}
