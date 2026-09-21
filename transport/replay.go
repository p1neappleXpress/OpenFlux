package transport

// replayFilter is a sliding-window anti-replay check over 64-bit packet
// counters (RFC 6479 style bitmap ring). It accepts every counter at most
// once, tolerates reordering within replayWindowSize counters behind the
// highest one seen, and rejects anything older than that.
//
// The zero value is ready to use. Not safe for concurrent use; the session
// guards it with its receive lock.
type replayFilter struct {
	last uint64
	ring [replayRingBlocks]uint64
}

const (
	replayBlockBitLog = 6 // 64-bit blocks
	replayBlockBits   = 1 << replayBlockBitLog
	replayRingBlocks  = 128
	replayBlockMask   = replayRingBlocks - 1
	replayBitMask     = replayBlockBits - 1
	// replayWindowSize is how far behind the newest counter a packet may
	// arrive and still be accepted. One block is sacrificed so the newest
	// block can always be cleared before use.
	replayWindowSize = (replayRingBlocks - 1) * replayBlockBits
)

// ValidateAndUpdate reports whether counter is fresh and, if so, records it.
func (f *replayFilter) ValidateAndUpdate(counter uint64) bool {
	indexBlock := counter >> replayBlockBitLog
	if counter > f.last {
		// Move the window forward, clearing every block the newest counter
		// skipped over (capped at the whole ring for big jumps).
		current := f.last >> replayBlockBitLog
		diff := indexBlock - current
		if diff > replayRingBlocks {
			diff = replayRingBlocks
		}
		for i := current + 1; i <= current+diff; i++ {
			f.ring[i&replayBlockMask] = 0
		}
		f.last = counter
	} else if f.last-counter >= replayWindowSize {
		return false
	}
	bit := uint64(1) << (counter & replayBitMask)
	block := &f.ring[indexBlock&replayBlockMask]
	if *block&bit != 0 {
		return false
	}
	*block |= bit
	return true
}
