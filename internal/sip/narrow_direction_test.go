package sip

import (
	"strconv"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/codec"
)

func TestNarrowDirection(t *testing.T) {
	tests := []struct {
		mirrored string
		max      string
		want     string
	}{
		// No cap: every existing leg keeps the mirrored direction untouched.
		{DirSendRecv, "", DirSendRecv},
		{DirSendOnly, "", DirSendOnly},
		{DirRecvOnly, "", DirRecvOnly},
		{DirInactive, "", DirInactive},

		// Capped at recvonly: a recording session never transmits.
		{DirSendRecv, DirRecvOnly, DirRecvOnly},
		{DirRecvOnly, DirRecvOnly, DirRecvOnly},
		{DirSendOnly, DirRecvOnly, DirInactive},
		{DirInactive, DirRecvOnly, DirInactive},
	}

	for _, tc := range tests {
		if got := NarrowDirection(tc.mirrored, tc.max); got != tc.want {
			t.Errorf("NarrowDirection(%q, %q) = %q, want %q", tc.mirrored, tc.max, got, tc.want)
		}
	}
}

// offerWithDirections builds a parsed offer carrying one m=audio per direction.
func offerWithDirections(t *testing.T, dirs ...string) *SDPMedia {
	t.Helper()
	sdp := "v=0\r\no=- 1 1 IN IP4 10.0.0.1\r\ns=-\r\nc=IN IP4 10.0.0.1\r\nt=0 0\r\n"
	for i, d := range dirs {
		sdp += "m=audio " + strconv.Itoa(40000+i*2) + " RTP/AVP 0\r\n" +
			"a=rtpmap:0 PCMU/8000\r\n" +
			"a=" + d + "\r\n" +
			"a=mid:" + strconv.Itoa(i) + "\r\n" +
			"a=label:" + strconv.Itoa(i+1) + "\r\n"
	}
	offer, err := ParseSDP([]byte(sdp))
	if err != nil {
		t.Fatalf("ParseSDP = %v, want nil", err)
	}
	return offer
}

func TestPlanAnswer_MaxDirectionForcesReceiveOnly(t *testing.T) {
	offer := offerWithDirections(t, DirSendOnly, DirSendRecv, DirRecvOnly)

	plans := PlanAnswer(offer, AnswerOptions{
		SupportedCodecs: []codec.CodecType{codec.CodecPCMU},
		TextMLineIndex:  -1,
		StrictMLines:    true,
		MaxDirection:    DirRecvOnly,
	})

	accepted := AcceptedAudio(plans)
	if len(accepted) != 3 {
		t.Fatalf("accepted %d sections, want 3", len(accepted))
	}
	want := []string{DirRecvOnly, DirRecvOnly, DirInactive}
	for i, p := range accepted {
		if p.Direction != want[i] {
			t.Errorf("section %d direction = %q, want %q", i, p.Direction, want[i])
		}
		if p.Label != strconv.Itoa(i+1) {
			t.Errorf("section %d label = %q, want %q", i, p.Label, strconv.Itoa(i+1))
		}
	}
}

func TestPlanAnswer_NoMaxDirectionIsUnchanged(t *testing.T) {
	offer := offerWithDirections(t, DirSendOnly, DirSendRecv, DirRecvOnly)

	plans := PlanAnswer(offer, AnswerOptions{
		SupportedCodecs: []codec.CodecType{codec.CodecPCMU},
		TextMLineIndex:  -1,
		StrictMLines:    true,
	})

	accepted := AcceptedAudio(plans)
	want := []string{DirRecvOnly, DirSendRecv, DirSendOnly}
	for i, p := range accepted {
		if p.Direction != want[i] {
			t.Errorf("section %d direction = %q, want %q (plain mirroring)", i, p.Direction, want[i])
		}
	}
}
