package control

import "testing"

func TestBondingAckRoundTrip(t *testing.T) {
	var a BondingAck
	for _, seq := range []uint64{1000, 999, 990, 489} {
		a.Set(seq)
	}
	a.Set(488) // older than the span: not recorded
	b, err := DecodeBondingAck(a.Encode())
	if err != nil {
		t.Fatal(err)
	}
	for _, seq := range []uint64{1000, 999, 990, 489} {
		if !b.Has(seq) {
			t.Errorf("seq %d missing after round trip", seq)
		}
	}
	for _, seq := range []uint64{998, 488, 1001, 0} {
		if b.Has(seq) {
			t.Errorf("seq %d reported but never set", seq)
		}
	}
	if b.Newest != 1000 {
		t.Errorf("newest %d", b.Newest)
	}
}

func TestBondingAckNewerShiftsMap(t *testing.T) {
	var a BondingAck
	a.Set(10)
	a.Set(300)
	a.Set(600) // 10 falls out of the 512-number span, 300 stays
	if a.Has(10) || !a.Has(300) || !a.Has(600) {
		t.Fatalf("after shift: has10=%v has300=%v has600=%v", a.Has(10), a.Has(300), a.Has(600))
	}
	a.Set(5000) // everything older is gone
	if a.Has(600) || !a.Has(5000) {
		t.Fatal("a jump past the span keeps old bits")
	}
}

func TestDecodeBondingAckRejectsLength(t *testing.T) {
	if _, err := DecodeBondingAck(make([]byte, 10)); err == nil {
		t.Fatal("short payload accepted")
	}
}
