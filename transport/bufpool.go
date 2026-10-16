package transport

import (
	"math/bits"
	"sync"
)

// A pool of byte buffers in power-of-two size classes for the send path. Every
// packet leaving a busy node used to be allocated three times in a row (the
// session's envelope+packet, the batch frame, the encrypted record), each up to
// 16 KB and each garbage a moment later: a third of the CPU of a loaded exit
// went to allocating, zeroing and collecting them. Ownership is passed along
// explicitly and the buffer is returned by whoever consumes it last (see the
// callers); a buffer that is never returned is simply garbage collected.

const (
	bufMinShift = 11 // 2 KiB
	bufMaxShift = 16 // 64 KiB
)

var bufPools [bufMaxShift - bufMinShift + 1]sync.Pool

// getBuf returns a buffer of length n whose contents are unspecified (the
// caller overwrites all of it) and whose capacity is a pool size class. Sizes
// beyond the largest class come straight from the allocator.
func getBuf(n int) []byte {
	if n <= 0 {
		return nil
	}
	shift := max(bits.Len(uint(n-1)), bufMinShift)
	if shift > bufMaxShift {
		return make([]byte, n)
	}
	if v := bufPools[shift-bufMinShift].Get(); v != nil {
		return (*(v.(*[]byte)))[:n]
	}
	return make([]byte, n, 1<<shift)
}

// putBuf returns a buffer obtained with getBuf (or any buffer whose capacity
// happens to be a size class: others are left to the collector). The caller
// must not touch b, or anything sliced from it, afterwards.
func putBuf(b []byte) {
	c := cap(b)
	if c < 1<<bufMinShift || c > 1<<bufMaxShift || c&(c-1) != 0 {
		return
	}
	b = b[:c]
	bufPools[bits.Len(uint(c))-1-bufMinShift].Put(&b)
}
