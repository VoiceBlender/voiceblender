package leg

import (
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
)

func TestConfigureAMRWBOctetAligned(t *testing.T) {
	l := &SIPLeg{codecType: codec.CodecAMRWB}
	remote := &sipmod.SDPMedia{
		CodecFmtp: map[codec.CodecType]string{codec.CodecAMRWB: "octet-align=1"},
	}
	l.configureAMRWB(remote, 97)

	if l.rtpSendPT != 97 {
		t.Errorf("rtpSendPT = %d, want 97 (remote PT)", l.rtpSendPT)
	}
	if !l.amrwbOctetAligned {
		t.Error("amrwbOctetAligned = false, want true for octet-align=1 peer")
	}
	// No mode-set ⇒ no clamp, no echo.
	if l.amrwbMode != defaultAMRWBEncoderMode {
		t.Errorf("amrwbMode = %d, want %d (default without engine)", l.amrwbMode, defaultAMRWBEncoderMode)
	}
	if l.amrwbModeSet != "" {
		t.Errorf("amrwbModeSet = %q, want empty (no peer mode-set)", l.amrwbModeSet)
	}
}

func TestConfigureAMRWBClampsToModeSet(t *testing.T) {
	// Peer restricts to mode-set 0,1,2; the default ceiling (8) clamps to 2,
	// and we echo the peer's mode-set in our answer.
	l := &SIPLeg{codecType: codec.CodecAMRWB}
	remote := &sipmod.SDPMedia{
		CodecFmtp: map[codec.CodecType]string{codec.CodecAMRWB: "octet-align=1; mode-set=0,1,2"},
	}
	l.configureAMRWB(remote, 97)

	if l.amrwbMode != 2 {
		t.Errorf("amrwbMode = %d, want 2 (clamped to peer mode-set)", l.amrwbMode)
	}
	if l.amrwbModeSet != "0,1,2" {
		t.Errorf("amrwbModeSet = %q, want 0,1,2 (echoed)", l.amrwbModeSet)
	}
}

func TestConfigureAMRWBBandwidthEfficient(t *testing.T) {
	l := &SIPLeg{codecType: codec.CodecAMRWB}
	// No octet-align param ⇒ RFC 4867 default (bandwidth-efficient).
	remote := &sipmod.SDPMedia{CodecFmtp: map[codec.CodecType]string{}}
	l.configureAMRWB(remote, 100)

	if l.rtpSendPT != 100 {
		t.Errorf("rtpSendPT = %d, want 100", l.rtpSendPT)
	}
	if l.amrwbOctetAligned {
		t.Error("amrwbOctetAligned = true, want false for peer without octet-align")
	}
}

func TestConfigureAMRWBNoOpForOtherCodecs(t *testing.T) {
	l := &SIPLeg{codecType: codec.CodecOpus}
	remote := &sipmod.SDPMedia{
		CodecFmtp: map[codec.CodecType]string{codec.CodecAMRWB: "octet-align=1"},
	}
	l.configureAMRWB(remote, 96)

	if l.rtpSendPT != 0 {
		t.Errorf("rtpSendPT = %d, want 0 (unchanged for non-AMR-WB)", l.rtpSendPT)
	}
	if l.amrwbOctetAligned {
		t.Error("amrwbOctetAligned set for a non-AMR-WB codec")
	}
}
