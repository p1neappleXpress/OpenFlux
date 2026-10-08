package transport

import (
	"sync"
	"testing"
	"time"
)

func TestReorderBufferOrderAndSkip(t *testing.T) {
	var mu sync.Mutex
	var got []byte
	r := newReorderBuffer(func(b []byte) { mu.Lock(); got = append(got, b[0]); mu.Unlock() })
	done := make(chan struct{})
	defer close(done)
	go r.run(done)

	r.push(10, []byte{10})
	r.push(12, []byte{12}) // 11 missing
	r.push(11, []byte{11}) // fills hole
	r.push(14, []byte{14}) // 13 never arrives -> skipped after timeout
	time.Sleep(500 * time.Millisecond)
	r.push(13, []byte{13}) // late, dropped
	r.push(15, []byte{15})
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	want := []byte{10, 11, 12, 14, 15}
	if string(got) != string(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	d, sk, late, _, _ := r.snapshot()
	if d != 5 || sk != 1 || late != 1 {
		t.Fatalf("delivered=%d skipped=%d late=%d", d, sk, late)
	}
}
