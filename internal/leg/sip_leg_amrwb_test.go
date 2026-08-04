package leg

import (
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
)

func TestConfigureAMRWBOctetAligned(t *testing.T) {
	l := newTestSIPLeg(codec.CodecAMRWB)
	remote := &sipmod.SDPMedia{
		CodecFmtp: map[codec.CodecType]string{codec.CodecAMRWB: "octet-align=1"},
	}
	l.configureAMRWB(l.prim, remote, 97)

	if l.prim.rtpSendPT != 97 {
		t.Errorf("rtpSendPT = %d, want 97 (remote PT)", l.prim.rtpSendPT)
	}
	if !l.prim.amrwbOctetAligned {
		t.Error("amrwbOctetAligned = false, want true for octet-align=1 peer")
	}
	// No mode-set ⇒ no clamp, no echo.
	if l.prim.amrwbMode != defaultAMRWBEncoderMode {
		t.Errorf("amrwbMode = %d, want %d (default without engine)", l.prim.amrwbMode, defaultAMRWBEncoderMode)
	}
	if l.prim.amrwbModeSet != "" {
		t.Errorf("amrwbModeSet = %q, want empty (no peer mode-set)", l.prim.amrwbModeSet)
	}
}

func TestConfigureAMRWBClampsToModeSet(t *testing.T) {
	// Peer restricts to mode-set 0,1,2; the default ceiling (8) clamps to 2,
	// and we echo the peer's mode-set in our answer.
	l := newTestSIPLeg(codec.CodecAMRWB)
	remote := &sipmod.SDPMedia{
		CodecFmtp: map[codec.CodecType]string{codec.CodecAMRWB: "octet-align=1; mode-set=0,1,2"},
	}
	l.configureAMRWB(l.prim, remote, 97)

	if l.prim.amrwbMode != 2 {
		t.Errorf("amrwbMode = %d, want 2 (clamped to peer mode-set)", l.prim.amrwbMode)
	}
	if l.prim.amrwbModeSet != "0,1,2" {
		t.Errorf("amrwbModeSet = %q, want 0,1,2 (echoed)", l.prim.amrwbModeSet)
	}
}

func TestConfigureAMRWBBandwidthEfficient(t *testing.T) {
	l := newTestSIPLeg(codec.CodecAMRWB)
	// No octet-align param ⇒ RFC 4867 default (bandwidth-efficient).
	remote := &sipmod.SDPMedia{CodecFmtp: map[codec.CodecType]string{}}
	l.configureAMRWB(l.prim, remote, 100)

	if l.prim.rtpSendPT != 100 {
		t.Errorf("rtpSendPT = %d, want 100", l.prim.rtpSendPT)
	}
	if l.prim.amrwbOctetAligned {
		t.Error("amrwbOctetAligned = true, want false for peer without octet-align")
	}
}

func TestConfigureAMRWBNoOpForOtherCodecs(t *testing.T) {
	l := newTestSIPLeg(codec.CodecOpus)
	remote := &sipmod.SDPMedia{
		CodecFmtp: map[codec.CodecType]string{codec.CodecAMRWB: "octet-align=1"},
	}
	l.configureAMRWB(l.prim, remote, 96)

	if l.prim.rtpSendPT != 0 {
		t.Errorf("rtpSendPT = %d, want 0 (unchanged for non-AMR-WB)", l.prim.rtpSendPT)
	}
	if l.prim.amrwbOctetAligned {
		t.Error("amrwbOctetAligned set for a non-AMR-WB codec")
	}
}
