package transport

import (
	"encoding/binary"
	"math"
	"testing"
)

// buildTCP builds a minimal IPv4+TCP packet with the given 5-tuple so the tests
// can exercise flowHash the way real traffic hits it.
func buildTCP(srcIP, dstIP [4]byte, srcPort, dstPort uint16) []byte {
	pkt := make([]byte, 40) // 20 IP + 20 TCP
	pkt[0] = 0x45           // IPv4, IHL=5
	pkt[9] = 6              // TCP
	copy(pkt[12:16], srcIP[:])
	copy(pkt[16:20], dstIP[:])
	binary.BigEndian.PutUint16(pkt[20:22], srcPort)
	binary.BigEndian.PutUint16(pkt[22:24], dstPort)
	return pkt
}

// TestFlowHashStable: every packet of one flow must hash to the same value,
// otherwise per-flow affinity breaks and a single TCP connection would be split
// across channels (reordering).
func TestFlowHashStable(t *testing.T) {
	a := buildTCP([4]byte{10, 0, 0, 2}, [4]byte{93, 184, 216, 34}, 51000, 443)
	b := buildTCP([4]byte{10, 0, 0, 2}, [4]byte{93, 184, 216, 34}, 51000, 443)
	if flowHash(a) != flowHash(b) {
		t.Fatalf("same flow hashed differently: %d vs %d", flowHash(a), flowHash(b))
	}

	// A different source port is a different flow and should (very likely) map
	// elsewhere; at minimum it must be allowed to differ.
	c := buildTCP([4]byte{10, 0, 0, 2}, [4]byte{93, 184, 216, 34}, 51001, 443)
	if flowHash(a) == flowHash(c) {
		t.Logf("note: distinct flows collided on hash (acceptable but rare): %d", flowHash(a))
	}
}

// TestFlowHashDistribution: many distinct flows over N channels should spread
// reasonably evenly (no channel starved, none hogging everything).
func TestFlowHashDistribution(t *testing.T) {
	const channels = 4
	const flows = 4000
	counts := make([]int, channels)

	for i := 0; i < flows; i++ {
		pkt := buildTCP(
			[4]byte{10, 0, 0, 2},
			[4]byte{93, 184, byte(i >> 8), byte(i)},
			uint16(40000+i%20000),
			443,
		)
		counts[flowHash(pkt)%channels]++
	}

	expected := float64(flows) / float64(channels)
	for ch, n := range counts {
		dev := math.Abs(float64(n)-expected) / expected
		if dev > 0.20 { // within 20% of even is plenty for a hash split
			t.Errorf("channel %d got %d flows, expected ~%.0f (%.0f%% off)", ch, n, expected, dev*100)
		}
	}
	t.Logf("distribution over %d channels: %v", channels, counts)
}

// TestFlowHashNonIPv4 must not panic on short or non-IPv4 input.
func TestFlowHashNonIPv4(t *testing.T) {
	flowHash([]byte{})
	flowHash([]byte{0x60, 0x00})       // IPv6-ish, too short
	flowHash([]byte{0x45})             // IPv4 nibble but truncated
	flowHash(make([]byte, 8))          // short
	flowHash(buildTCP([4]byte{1, 1, 1, 1}, [4]byte{2, 2, 2, 2}, 1, 2)[:22]) // TCP header truncated
}
