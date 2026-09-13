package transport

import (
	"encoding/base64"
	"math/rand"
	"testing"
)

// The fixed envelope the Yandex transport wraps every channel message in:
//   42["message",{"type":"cursor","cursor":"18;<base64>"}]
const yandexEnvelope = len(`42["message",{"type":"cursor","cursor":"18;`) + len(`"}]`)

func b64len(n int) int { return base64.StdEncoding.EncodedLen(n) }

// mkPkt builds a realistic IPv4+TCP packet: constant 4-tuple (one flow),
// advancing seq/ack, optional payload. Headers repeat across a flow, which is
// exactly what batch compression exploits.
func mkPkt(seq, ack uint32, payload []byte) []byte {
	p := make([]byte, 40+len(payload))
	p[0] = 0x45
	total := 40 + len(payload)
	p[2], p[3] = byte(total>>8), byte(total)
	p[8], p[9] = 64, 6
	copy(p[12:16], []byte{10, 10, 10, 2})
	copy(p[16:20], []byte{93, 184, 216, 34})
	copy(p[20:22], []byte{0xab, 0xcd})
	copy(p[22:24], []byte{0x01, 0xbb})
	p[24], p[25], p[26], p[27] = byte(seq>>24), byte(seq>>16), byte(seq>>8), byte(seq)
	p[28], p[29], p[30], p[31] = byte(ack>>24), byte(ack>>16), byte(ack>>8), byte(ack)
	p[32] = 0x50
	p[33] = 0x10
	copy(p[34:36], []byte{0xff, 0xff})
	copy(p[40:], payload)
	return p
}

// legacyWire: old path — each packet compressed alone, base64'd, one message.
func legacyWire(trace [][]byte) (bytes, msgs int) {
	for _, pkt := range trace {
		c := compress(pkt)
		bytes += yandexEnvelope + b64len(len(c))
		msgs++
	}
	return
}

// batchedWire: new path — coalesce into batches (same caps as BatchedTransport),
// encode+compress the batch, base64, one message per batch.
func batchedWire(trace [][]byte) (bytes, msgs int) {
	i := 0
	for i < len(trace) {
		batch := [][]byte{trace[i]}
		size := 2 + len(trace[i])
		i++
		for i < len(trace) && size < defaultMaxBatchBytes && len(batch) < defaultMaxBatchCount {
			size += 2 + len(trace[i])
			batch = append(batch, trace[i])
			i++
		}
		w := encodeBatch(batch)
		bytes += yandexEnvelope + b64len(len(w))
		msgs++
	}
	return
}

func reportEfficiency(t *testing.T, name string, trace [][]byte) (msgGain, byteGain float64) {
	t.Helper()
	ob, om := legacyWire(trace)
	nb, nm := batchedWire(trace)
	msgGain = float64(om) / float64(nm)
	byteGain = float64(ob) / float64(nb)
	t.Logf("%s (%d tunnel packets)", name, len(trace))
	t.Logf("   legacy : %6d channel msgs, %9d wire bytes", om, ob)
	t.Logf("   batched: %6d channel msgs, %9d wire bytes", nm, nb)
	t.Logf("   gain   : %.1fx fewer messages, %.2fx fewer wire bytes", msgGain, byteGain)
	return
}

func TestWireEfficiencyBulkDownload(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	var trace [][]byte
	var seq uint32 = 1000
	for i := 0; i < 1000; i++ {
		payload := make([]byte, 1460) // encrypted app data => incompressible
		rng.Read(payload)
		trace = append(trace, mkPkt(seq, 5000, payload))
		seq += 1460
	}
	msgGain, byteGain := reportEfficiency(t, "BULK DOWNLOAD (1460B encrypted segments)", trace)
	if msgGain < 3 {
		t.Errorf("expected >=3x fewer messages on bulk, got %.1fx", msgGain)
	}
	if byteGain < 1.0 {
		t.Errorf("batched should not inflate bulk wire bytes, got %.2fx", byteGain)
	}
}

func TestWireEfficiencyAckHeavy(t *testing.T) {
	var trace [][]byte
	var ack uint32 = 1000
	for i := 0; i < 1000; i++ {
		trace = append(trace, mkPkt(42, ack, nil)) // pure 40-byte ACKs
		ack += 1460
	}
	msgGain, byteGain := reportEfficiency(t, "ACK-HEAVY / INTERACTIVE (40B headers)", trace)
	if msgGain < 20 {
		t.Errorf("expected >=20x fewer messages on ACK flood, got %.1fx", msgGain)
	}
	if byteGain < 5 {
		t.Errorf("expected >=5x fewer wire bytes on ACK flood, got %.2fx", byteGain)
	}
}
