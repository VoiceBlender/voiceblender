package leg

import (
	"testing"
	"time"

	"github.com/pion/rtp"

	"github.com/VoiceBlender/voiceblender/internal/codec"
)

// feedRTP makes readLoop's two calls for an audio packet.
func feedRTP(l *SIPLeg, s *mediaStream, ssrc uint32, seq uint16, ts uint32, at time.Time) {
	pkt := &rtp.Packet{Header: rtp.Header{Version: 2, SSRC: ssrc, SequenceNumber: seq, Timestamp: ts}}
	l.trackRTPSeq(s, &pkt.Header)
	l.updateRTPJitter(s, &liveCodec{codecType: codec.CodecPCMU}, pkt, at)
}

// One second at 8 kHz; a carried-over transit would add ~6.7e7.
const jitterCeiling = 8000

func TestUpdateRTPJitterResetsOnNewSSRC(t *testing.T) {
	l, s := &SIPLeg{}, &mediaStream{}
	now := time.Now()
	feedRTP(l, s, 1, 10, 0, now)
	feedRTP(l, s, 2, 500, 1<<30, now.Add(20*time.Millisecond))
	feedRTP(l, s, 2, 501, 1<<30+160, now.Add(40*time.Millisecond))
	if s.rtpJitter > jitterCeiling {
		t.Fatalf("jitter %.0f after an SSRC change", s.rtpJitter)
	}
}

func TestUpdateRTPJitterResetsOnSequenceRestart(t *testing.T) {
	l, s := &SIPLeg{}, &mediaStream{}
	now := time.Now()
	for i := 0; i < 10; i++ {
		feedRTP(l, s, 1, uint16(100+i), uint32(i)*160, now.Add(time.Duration(i)*20*time.Millisecond))
	}
	// Same SSRC, new source: sequence and timestamp both jump.
	for i := 10; i < 20; i++ {
		feedRTP(l, s, 1, uint16(30_000+i), 1<<30+uint32(i)*160, now.Add(time.Duration(i)*20*time.Millisecond))
	}
	if s.rtpJitter > jitterCeiling {
		t.Fatalf("jitter %.0f after a sequence restart", s.rtpJitter)
	}
	if got := s.rtpRecv.Run(); got != 2 {
		t.Fatalf("run = %d, want 2", got)
	}
}

func TestUpdateRTPJitterIgnoresStray(t *testing.T) {
	l, s := &SIPLeg{}, &mediaStream{}
	now := time.Now()
	for i := 0; i < 10; i++ {
		feedRTP(l, s, 1, uint16(i), uint32(i)*160, now.Add(time.Duration(i)*20*time.Millisecond))
	}
	feedRTP(l, s, 1, 20_000, 1<<30, now.Add(200*time.Millisecond))
	feedRTP(l, s, 1, 10, 10*160, now.Add(200*time.Millisecond))
	if s.rtpJitter > jitterCeiling {
		t.Fatalf("jitter %.0f after a stray packet", s.rtpJitter)
	}
}

func TestRTPArrival(t *testing.T) {
	if got := rtpArrival(time.Unix(1, 500_000_000), 8000); got != 12_000 {
		t.Errorf("1.5 s at 8 kHz = %d, want 12000", got)
	}
	// UnixNano()*8000 overflows int64 between these two instants.
	t0 := time.Unix(0, (1<<63)/8000-10_000_000)
	t1 := t0.Add(20 * time.Millisecond)
	for _, rate := range []int64{8000, 48000} {
		if d := rtpArrival(t1, rate) - rtpArrival(t0, rate); d != rate/50 {
			t.Errorf("20 ms at %d Hz = %d units, want %d", rate, d, rate/50)
		}
	}
	now := time.Now()
	if d := rtpArrival(now.Add(time.Second), 48000) - rtpArrival(now, 48000); d < 47_999 || d > 48_001 {
		t.Errorf("1 s at 48 kHz now = %d units", d)
	}
}
