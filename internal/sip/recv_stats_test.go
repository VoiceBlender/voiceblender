package sip

import "testing"

func checkRecv(t *testing.T, r *RecvStats, received, expected, lost uint32) {
	t.Helper()
	if got := r.Received(); got != received {
		t.Errorf("received = %d, want %d", got, received)
	}
	if got := r.Expected(); got != expected {
		t.Errorf("expected = %d, want %d", got, expected)
	}
	if got := r.Lost(); got != lost {
		t.Errorf("lost = %d, want %d", got, lost)
	}
}

func TestRecvStatsZero(t *testing.T) {
	var r RecvStats
	checkRecv(t, &r, 0, 0, 0)
}

func TestRecvStatsInOrderWithLoss(t *testing.T) {
	var r RecvStats
	for seq := uint16(1000); seq < 1100; seq++ {
		if seq%10 == 3 {
			continue
		}
		r.Update(7, seq)
	}
	checkRecv(t, &r, 90, 100, 10)
}

func TestRecvStatsSequenceWrap(t *testing.T) {
	var r RecvStats
	const total = 200_000 // three wraps
	seq := uint16(65_000)
	var lost uint32
	for i := 0; i < total; i++ {
		if i%100 == 50 {
			lost++
		} else {
			r.Update(7, seq)
		}
		seq++
	}
	checkRecv(t, &r, total-lost, total, lost)
}

func TestRecvStatsWrapOnLostPacket(t *testing.T) {
	var r RecvStats
	for _, seq := range []uint16{65533, 65534, 1, 2} { // 65535 and 0 lost
		r.Update(7, seq)
	}
	checkRecv(t, &r, 4, 6, 2)
}

func TestRecvStatsReorderAcrossWrap(t *testing.T) {
	var r RecvStats
	for _, seq := range []uint16{65534, 0, 65535, 1} {
		r.Update(7, seq)
	}
	checkRecv(t, &r, 4, 4, 0)
}

func TestRecvStatsLatePacketDoesNotShrinkExpected(t *testing.T) {
	var r RecvStats
	for _, seq := range []uint16{10, 11, 13, 14, 12} {
		r.Update(7, seq)
	}
	checkRecv(t, &r, 5, 5, 0)
}

func TestRecvStatsDuplicatesClampLoss(t *testing.T) {
	var r RecvStats
	for _, seq := range []uint16{10, 11, 11, 11, 12} {
		r.Update(7, seq)
	}
	checkRecv(t, &r, 5, 3, 0)
}

func TestRecvStatsNewSSRCKeepsTotals(t *testing.T) {
	var r RecvStats
	for seq := uint16(100); seq < 200; seq++ {
		if seq%20 == 10 {
			continue
		}
		r.Update(1, seq)
	}
	checkRecv(t, &r, 95, 100, 5)
	for seq := uint16(40_000); seq < 40_050; seq++ {
		r.Update(2, seq)
	}
	checkRecv(t, &r, 145, 150, 5)
	if r.Run() != 2 || r.SSRC() != 2 || r.RunExpected() != 50 {
		t.Errorf("run %d ssrc %d run expected %d, want 2 2 50", r.Run(), r.SSRC(), r.RunExpected())
	}
}

func TestRecvStatsConfirmedJumpRestarts(t *testing.T) {
	var r RecvStats
	for seq := uint16(100); seq < 200; seq++ {
		r.Update(1, seq)
	}
	if r.Update(1, 30_000) {
		t.Error("first packet of a large jump was counted")
	}
	checkRecv(t, &r, 100, 100, 0)
	if r.Run() != 1 {
		t.Errorf("run = %d before the jump is confirmed, want 1", r.Run())
	}
	for seq := uint16(30_001); seq < 30_051; seq++ {
		if !r.Update(1, seq) {
			t.Fatalf("seq %d not counted", seq)
		}
	}
	checkRecv(t, &r, 150, 150, 0)
	if r.Run() != 2 {
		t.Errorf("run = %d after a confirmed jump, want 2", r.Run())
	}
}

func TestRecvStatsLoneStrayIgnored(t *testing.T) {
	var r RecvStats
	for seq := uint16(100); seq < 150; seq++ {
		r.Update(1, seq)
	}
	if r.Update(1, 20_000) {
		t.Error("stray packet was counted")
	}
	for seq := uint16(150); seq < 200; seq++ {
		r.Update(1, seq)
	}
	checkRecv(t, &r, 100, 100, 0)
	if r.Run() != 1 {
		t.Errorf("run = %d after a lone stray, want 1", r.Run())
	}
}

func TestRecvStatsInterval(t *testing.T) {
	var r RecvStats
	for seq := uint16(0); seq < 50; seq++ {
		if seq != 7 {
			r.Update(1, seq)
		}
	}
	if exp, rcv := r.Interval(); exp != 50 || rcv != 49 {
		t.Errorf("first interval %d/%d, want 50/49", exp, rcv)
	}
	if exp, rcv := r.Interval(); exp != 0 || rcv != 0 {
		t.Errorf("empty interval %d/%d, want 0/0", exp, rcv)
	}
	for seq := uint16(50); seq < 60; seq++ {
		r.Update(1, seq)
	}
	if exp, rcv := r.Interval(); exp != 10 || rcv != 10 {
		t.Errorf("second interval %d/%d, want 10/10", exp, rcv)
	}
	// A new run starts its intervals afresh.
	for seq := uint16(9000); seq < 9005; seq++ {
		r.Update(2, seq)
	}
	if exp, rcv := r.Interval(); exp != 5 || rcv != 5 {
		t.Errorf("interval after a new SSRC %d/%d, want 5/5", exp, rcv)
	}
}
