package yandex

import (
	"net/http"
	"sync/atomic"
	"testing"
)

// Manager.Stop stops each transport after Session.Stop already did; the
// second Stop, and a Send racing with Stop, must not panic on the closed
// queues.
func TestRelayStopTwiceAndSendAfterStop(t *testing.T) {
	cfg := DefaultVolgaConfig()
	var auth atomic.Pointer[volgaAuth]
	auth.Store(&volgaAuth{Session: &http.Client{}})
	r := newRelayClient(&auth, cfg, &VolgaStats{})
	r.Start()
	r.Stop()
	r.Stop()
	if err := r.Send([]byte{1, 2, 3}); err == nil {
		t.Fatal("Send after Stop must fail")
	}
}
