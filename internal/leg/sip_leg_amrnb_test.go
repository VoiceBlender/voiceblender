package leg

import (
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
)

func TestConfigureAMRNBOctetAligned(t *testing.T) {
	l := newTestSIPLeg(codec.CodecAMRNB)
	remote := &sipmod.SDPMedia{
		CodecFmtp: map[codec.CodecType]string{codec.CodecAMRNB: "octet-align=1"},
	}
	l.configureAMRNB(l.prim, remote, 97)

	if l.prim.rtpSendPT != 97 {
		t.Errorf("rtpSendPT = %d, want 97 (remote PT)", l.prim.rtpSendPT)
	}
	if !l.prim.amrnbOctetAligned {
		t.Error("amrnbOctetAligned = false, want true for octet-align=1 peer")
	}
	if l.prim.amrnbMode != defaultAMRNBEncoderMode {
		t.Errorf("amrnbMode = %d, want %d (default without engine)", l.prim.amrnbMode, defaultAMRNBEncoderMode)
	}
	if l.prim.amrnbModeSet != "" {
		t.Errorf("amrnbModeSet = %q, want empty (no peer mode-set)", l.prim.amrnbModeSet)
	}
}

func TestConfigureAMRNBClampsToModeSet(t *testing.T) {
	// Peer restricts to mode-set 0,4; the default ceiling (7) clamps to 4 and
	// we echo the peer's mode-set back in our answer.
	l := newTestSIPLeg(codec.CodecAMRNB)
	remote := &sipmod.SDPMedia{
		CodecFmtp: map[codec.CodecType]string{codec.CodecAMRNB: "octet-align=1; mode-set=0,4"},
	}
	l.configureAMRNB(l.prim, remote, 97)

	if l.prim.amrnbMode != 4 {
		t.Errorf("amrnbMode = %d, want 4 (clamped to peer mode-set)", l.prim.amrnbMode)
	}
	if l.prim.amrnbModeSet != "0,4" {
		t.Errorf("amrnbModeSet = %q, want 0,4 (echoed)", l.prim.amrnbModeSet)
	}
}

func TestConfigureAMRNBBandwidthEfficient(t *testing.T) {
	l := newTestSIPLeg(codec.CodecAMRNB)
	remote := &sipmod.SDPMedia{CodecFmtp: map[codec.CodecType]string{}}
	l.configureAMRNB(l.prim, remote, 100)

	if l.prim.rtpSendPT != 100 {
		t.Errorf("rtpSendPT = %d, want 100", l.prim.rtpSendPT)
	}
	if l.prim.amrnbOctetAligned {
		t.Error("amrnbOctetAligned = true, want false for peer without octet-align")
	}
}

func TestConfigureAMRNBNoOpForOtherCodecs(t *testing.T) {
	l := newTestSIPLeg(codec.CodecOpus)
	remote := &sipmod.SDPMedia{
		CodecFmtp: map[codec.CodecType]string{codec.CodecAMRNB: "octet-align=1"},
	}
	l.configureAMRNB(l.prim, remote, 97)

	if l.prim.rtpSendPT != 0 {
		t.Errorf("rtpSendPT = %d, want 0 (unchanged for non-AMR-NB)", l.prim.rtpSendPT)
	}
	if l.prim.amrnbOctetAligned {
		t.Error("amrnbOctetAligned set for a non-AMR-NB codec")
	}
}
