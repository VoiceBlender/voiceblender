package leg

import (
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
)

func TestConfigureAMRNBOctetAligned(t *testing.T) {
	l := &SIPLeg{codecType: codec.CodecAMRNB}
	remote := &sipmod.SDPMedia{
		CodecFmtp: map[codec.CodecType]string{codec.CodecAMRNB: "octet-align=1"},
	}
	l.configureAMRNB(remote, 97)

	if l.rtpSendPT != 97 {
		t.Errorf("rtpSendPT = %d, want 97 (remote PT)", l.rtpSendPT)
	}
	if !l.amrnbOctetAligned {
		t.Error("amrnbOctetAligned = false, want true for octet-align=1 peer")
	}
	if l.amrnbMode != defaultAMRNBEncoderMode {
		t.Errorf("amrnbMode = %d, want %d (default without engine)", l.amrnbMode, defaultAMRNBEncoderMode)
	}
	if l.amrnbModeSet != "" {
		t.Errorf("amrnbModeSet = %q, want empty (no peer mode-set)", l.amrnbModeSet)
	}
}

func TestConfigureAMRNBClampsToModeSet(t *testing.T) {
	// Peer restricts to mode-set 0,4; the default ceiling (7) clamps to 4 and
	// we echo the peer's mode-set back in our answer.
	l := &SIPLeg{codecType: codec.CodecAMRNB}
	remote := &sipmod.SDPMedia{
		CodecFmtp: map[codec.CodecType]string{codec.CodecAMRNB: "octet-align=1; mode-set=0,4"},
	}
	l.configureAMRNB(remote, 97)

	if l.amrnbMode != 4 {
		t.Errorf("amrnbMode = %d, want 4 (clamped to peer mode-set)", l.amrnbMode)
	}
	if l.amrnbModeSet != "0,4" {
		t.Errorf("amrnbModeSet = %q, want 0,4 (echoed)", l.amrnbModeSet)
	}
}

func TestConfigureAMRNBBandwidthEfficient(t *testing.T) {
	l := &SIPLeg{codecType: codec.CodecAMRNB}
	remote := &sipmod.SDPMedia{CodecFmtp: map[codec.CodecType]string{}}
	l.configureAMRNB(remote, 100)

	if l.rtpSendPT != 100 {
		t.Errorf("rtpSendPT = %d, want 100", l.rtpSendPT)
	}
	if l.amrnbOctetAligned {
		t.Error("amrnbOctetAligned = true, want false for peer without octet-align")
	}
}

func TestConfigureAMRNBNoOpForOtherCodecs(t *testing.T) {
	l := &SIPLeg{codecType: codec.CodecOpus}
	remote := &sipmod.SDPMedia{
		CodecFmtp: map[codec.CodecType]string{codec.CodecAMRNB: "octet-align=1"},
	}
	l.configureAMRNB(remote, 97)

	if l.rtpSendPT != 0 {
		t.Errorf("rtpSendPT = %d, want 0 (unchanged for non-AMR-NB)", l.rtpSendPT)
	}
	if l.amrnbOctetAligned {
		t.Error("amrnbOctetAligned set for a non-AMR-NB codec")
	}
}
