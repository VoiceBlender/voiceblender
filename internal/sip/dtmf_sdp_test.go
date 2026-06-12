package sip

import (
	"strings"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/codec"
)

func TestTelephoneEventClockRate(t *testing.T) {
	cases := map[codec.CodecType]int{
		codec.CodecAMRWB: 16000,
		codec.CodecOpus:  8000,
		codec.CodecPCMU:  8000,
		codec.CodecPCMA:  8000,
		// G.722 samples at 16kHz but its RTP/SDP clock is 8kHz (RFC 3551), so
		// telephone-event must pair at 8kHz — not the 16kHz sample rate.
		codec.CodecG722: 8000,
	}
	for c, want := range cases {
		if got := TelephoneEventClockRate(c); got != want {
			t.Errorf("TelephoneEventClockRate(%v) = %d, want %d", c, got, want)
		}
	}
}

// An AMR-WB answer must echo telephone-event at 16kHz to match the offer; a
// mismatched 8kHz makes strict peers (e.g. MicroSIP) drop DTMF.
func TestGenerateAnswerAMRWBTelephoneEvent16k(t *testing.T) {
	ans := string(GenerateAnswer(SDPConfig{
		LocalIP: "192.0.2.1",
		RTPPort: 5004,
		Codecs:  []codec.CodecType{codec.CodecAMRWB},
	}, codec.CodecAMRWB, 96, false))

	if !strings.Contains(ans, "telephone-event/16000") {
		t.Errorf("AMR-WB answer missing telephone-event/16000:\n%s", ans)
	}
	if strings.Contains(ans, "telephone-event/8000") {
		t.Errorf("AMR-WB answer should not carry telephone-event/8000:\n%s", ans)
	}
}

func TestGenerateAnswerNarrowbandTelephoneEvent8k(t *testing.T) {
	ans := string(GenerateAnswer(SDPConfig{
		LocalIP: "192.0.2.1",
		RTPPort: 5004,
		Codecs:  []codec.CodecType{codec.CodecPCMU},
	}, codec.CodecPCMU, 0, false))

	if !strings.Contains(ans, "telephone-event/8000") {
		t.Errorf("PCMU answer missing telephone-event/8000:\n%s", ans)
	}
}

// G.722 encodes at 16kHz but its RTP clock is 8kHz (RFC 3551), so its
// telephone-event must stay at 8kHz; using the 16kHz sample rate here would
// break DTMF the same way the AMR-WB bug did.
func TestGenerateAnswerG722TelephoneEvent8k(t *testing.T) {
	ans := string(GenerateAnswer(SDPConfig{
		LocalIP: "192.0.2.1",
		RTPPort: 5004,
		Codecs:  []codec.CodecType{codec.CodecG722},
	}, codec.CodecG722, 9, false))

	if !strings.Contains(ans, "telephone-event/8000") {
		t.Errorf("G722 answer missing telephone-event/8000:\n%s", ans)
	}
	if strings.Contains(ans, "telephone-event/16000") {
		t.Errorf("G722 answer should not carry telephone-event/16000:\n%s", ans)
	}
}

func TestGenerateReInviteAMRWBTelephoneEvent16k(t *testing.T) {
	sdp := string(GenerateReInviteSDP(SDPConfig{
		LocalIP: "192.0.2.1",
		RTPPort: 5004,
		Codecs:  []codec.CodecType{codec.CodecAMRWB},
	}, codec.CodecAMRWB, 96, "sendonly"))

	if !strings.Contains(sdp, "telephone-event/16000") {
		t.Errorf("AMR-WB re-INVITE missing telephone-event/16000:\n%s", sdp)
	}
}

// An AMR-WB-preferred offer advertises telephone-event at 16kHz.
func TestGenerateOfferAMRWBTelephoneEvent16k(t *testing.T) {
	offer := string(GenerateOffer(SDPConfig{
		LocalIP: "192.0.2.1",
		RTPPort: 5004,
		Codecs:  []codec.CodecType{codec.CodecAMRWB},
	}))
	if !strings.Contains(offer, "telephone-event/16000") {
		t.Errorf("AMR-WB offer missing telephone-event/16000:\n%s", offer)
	}
}

func TestParseSDPCapturesTelephoneEvent(t *testing.T) {
	// The MicroSIP offer from the bug report: AMR-WB with telephone-event/16000.
	raw := "v=0\r\n" +
		"o=- 1 1 IN IP4 192.0.2.1\r\n" +
		"s=-\r\n" +
		"c=IN IP4 192.0.2.1\r\n" +
		"t=0 0\r\n" +
		"m=audio 4002 RTP/AVP 96 101\r\n" +
		"a=rtpmap:96 AMR-WB/16000\r\n" +
		"a=fmtp:96 octet-align=1\r\n" +
		"a=rtpmap:101 telephone-event/16000\r\n" +
		"a=fmtp:101 0-16\r\n"

	m, err := ParseSDP([]byte(raw))
	if err != nil {
		t.Fatalf("ParseSDP: %v", err)
	}
	if got := m.DTMFEventPTs[101]; got != 16000 {
		t.Errorf("telephone-event PT 101 rate = %d, want 16000", got)
	}
	pt, ok := m.DTMFPTForRate(16000)
	if !ok || pt != 101 {
		t.Errorf("DTMFPTForRate(16000) = (%d, %v), want (101, true)", pt, ok)
	}
	if _, ok := m.DTMFPTForRate(8000); ok {
		t.Errorf("DTMFPTForRate(8000) should not match")
	}
}
