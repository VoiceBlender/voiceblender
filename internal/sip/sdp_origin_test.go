package sip

import (
	"strings"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/codec"
)

func sdpOriginFields(t *testing.T, sdp []byte) (id, version string) {
	t.Helper()
	for _, line := range strings.Split(string(sdp), "\r\n") {
		if f := strings.Fields(strings.TrimPrefix(line, "o=")); strings.HasPrefix(line, "o=") && len(f) >= 3 {
			return f[1], f[2]
		}
	}
	t.Fatalf("no o= line in:\n%s", sdp)
	return "", ""
}

// RFC 3264 §8: one session id for the whole session, and the version moves
// exactly when the SDP does.
func TestSDPOrigin_StableIDVersionTracksChanges(t *testing.T) {
	o := NewSDPOrigin()
	cfg := SDPConfig{LocalIP: "192.0.2.1", RTPPort: 4000, Codecs: []codec.CodecType{codec.CodecPCMU, codec.CodecG722}, Origin: o}

	id0, v0 := sdpOriginFields(t, GenerateOffer(cfg))
	id1, v1 := sdpOriginFields(t, GenerateAnswer(cfg, codec.CodecPCMU, 0, false))
	id2, v2 := sdpOriginFields(t, GenerateAnswer(cfg, codec.CodecPCMU, 0, false))
	id3, v3 := sdpOriginFields(t, GenerateReInviteSDP(cfg, codec.CodecPCMU, 0, DirSendOnly))
	id4, v4 := sdpOriginFields(t, GenerateReInviteSDP(cfg, codec.CodecPCMU, 0, DirSendOnly))

	if id1 != id0 || id2 != id0 || id3 != id0 || id4 != id0 {
		t.Errorf("session id changed: %s %s %s %s %s", id0, id1, id2, id3, id4)
	}
	if v0 != "0" || v1 != "1" {
		t.Errorf("versions %s -> %s, want 0 -> 1 for a changed SDP", v0, v1)
	}
	if v2 != v1 {
		t.Errorf("version moved for an identical SDP: %s -> %s", v1, v2)
	}
	if v3 != "2" || v4 != "2" {
		t.Errorf("hold re-offer versions %s, %s; want 2, 2", v3, v4)
	}
}

func TestSDPOrigin_NilIsOneOff(t *testing.T) {
	cfg := SDPConfig{LocalIP: "192.0.2.1", RTPPort: 4000, Codecs: []codec.CodecType{codec.CodecPCMU}}
	if _, v := sdpOriginFields(t, GenerateOffer(cfg)); v != "0" {
		t.Errorf("version %s, want 0", v)
	}
}
