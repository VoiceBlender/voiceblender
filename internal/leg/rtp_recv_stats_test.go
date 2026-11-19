package leg

import "testing"

func checkRecv(t *testing.T, r *rtpRecvStats, received, expected, lost uint32) {
	t.Helper()
	if got := r.received(); got != received {
		t.Errorf("received = %d, want %d", got, received)
	}
	if got := r.expected(); got != expected {
		t.Errorf("expected = %d, want %d", got, expected)
	}
	if got := r.lost(); got != lost {
		t.Errorf("lost = %d, want %d", got, lost)
	}
}

func TestRTPRecvStatsZero(t *testing.T) {
	var r rtpRecvStats
	checkRecv(t, &r, 0, 0, 0)
}

func TestRTPRecvStatsInOrderWithLoss(t *testing.T) {
	var r rtpRecvStats
	for seq := uint16(1000); seq < 1100; seq++ {
		if seq%10 == 3 {
			continue
		}
		r.update(7, seq)
	}
	checkRecv(t, &r, 90, 100, 10)
}

func TestRTPRecvStatsSequenceWrap(t *testing.T) {
	var r rtpRecvStats
	const total = 200_000 // three wraps
	seq := uint16(65_000)
	var lost uint32
	for i := 0; i < total; i++ {
		if i%100 == 50 {
			lost++
		} else {
			r.update(7, seq)
		}
		seq++
	}
	checkRecv(t, &r, total-lost, total, lost)
}

func TestRTPRecvStatsWrapOnLostPacket(t *testing.T) {
	var r rtpRecvStats
	for _, seq := range []uint16{65533, 65534, 1, 2} { // 65535 and 0 lost
		r.update(7, seq)
	}
	checkRecv(t, &r, 4, 6, 2)
}

func TestRTPRecvStatsReorderAcrossWrap(t *testing.T) {
	var r rtpRecvStats
	for _, seq := range []uint16{65534, 0, 65535, 1} {
		r.update(7, seq)
	}
	checkRecv(t, &r, 4, 4, 0)
}

func TestRTPRecvStatsLatePacketDoesNotShrinkExpected(t *testing.T) {
	var r rtpRecvStats
	for _, seq := range []uint16{10, 11, 13, 14, 12} {
		r.update(7, seq)
	}
	checkRecv(t, &r, 5, 5, 0)
}

func TestRTPRecvStatsDuplicatesClampLoss(t *testing.T) {
	var r rtpRecvStats
	for _, seq := range []uint16{10, 11, 11, 11, 12} {
		r.update(7, seq)
	}
	checkRecv(t, &r, 5, 3, 0)
}

func TestRTPRecvStatsNewSSRCKeepsTotals(t *testing.T) {
	var r rtpRecvStats
	for seq := uint16(100); seq < 200; seq++ {
		if seq%20 == 10 {
			continue
		}
		r.update(1, seq)
	}
	checkRecv(t, &r, 95, 100, 5)
	for seq := uint16(40_000); seq < 40_050; seq++ {
		r.update(2, seq)
	}
	checkRecv(t, &r, 145, 150, 5)
}

func TestRTPRecvStatsConfirmedJumpRestarts(t *testing.T) {
	var r rtpRecvStats
	for seq := uint16(100); seq < 200; seq++ {
		r.update(1, seq)
	}
	if r.update(1, 30_000) {
		t.Fatal("first packet of a large jump was counted")
	}
	for seq := uint16(30_001); seq < 30_051; seq++ {
		if !r.update(1, seq) {
			t.Fatalf("seq %d not counted", seq)
		}
	}
	checkRecv(t, &r, 150, 150, 0)
}

func TestRTPRecvStatsLoneStrayIgnored(t *testing.T) {
	var r rtpRecvStats
	for seq := uint16(100); seq < 150; seq++ {
		r.update(1, seq)
	}
	r.update(1, 20_000) // stray
	for seq := uint16(150); seq < 200; seq++ {
		r.update(1, seq)
	}
	checkRecv(t, &r, 100, 100, 0)
}
