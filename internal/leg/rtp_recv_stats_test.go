package leg

import (
	"testing"

	"github.com/pion/rtp"

	"github.com/VoiceBlender/voiceblender/internal/codec"
)

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
	r.update(1, 30_000)
	checkRecv(t, &r, 100, 100, 0)
	for seq := uint16(30_001); seq < 30_051; seq++ {
		r.update(1, seq)
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

func TestTrackRTPSeqSkipsNonV2(t *testing.T) {
	l, s := &SIPLeg{}, &mediaStream{}
	l.trackRTPSeq(s, &rtp.Header{Version: 0, SSRC: 1, SequenceNumber: 1})
	if got := s.rtpRecv.received(); got != 0 {
		t.Fatalf("version 0 packet counted: received = %d", got)
	}
	l.trackRTPSeq(s, &rtp.Header{Version: 2, SSRC: 1, SequenceNumber: 1})
	if got := s.rtpRecv.received(); got != 1 {
		t.Fatalf("version 2 packet not counted: received = %d", got)
	}
}

func TestUpdateRTPJitterResetsOnNewSSRC(t *testing.T) {
	l, s := &SIPLeg{}, &mediaStream{}
	lc := &liveCodec{codecType: codec.CodecPCMU}
	l.updateRTPJitter(s, lc, &rtp.Packet{Header: rtp.Header{SSRC: 1, Timestamp: 0}})
	l.updateRTPJitter(s, lc, &rtp.Packet{Header: rtp.Header{SSRC: 2, Timestamp: 1 << 30}})
	if s.rtpJitter > 8000 { // one second at 8 kHz; the old SSRC would add ~6.7e7
		t.Fatalf("jitter %.0f after SSRC change, want the new sender to start fresh", s.rtpJitter)
	}
	l.updateRTPJitter(s, lc, &rtp.Packet{Header: rtp.Header{SSRC: 2, Timestamp: 1<<30 + 160}})
	if s.rtpJitter > 8000 {
		t.Fatalf("jitter %.0f on the new SSRC's second packet", s.rtpJitter)
	}
}
