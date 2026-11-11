package leg

import (
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
)

func TestRTCPMux_FollowsOfferAnswer(t *testing.T) {
	muxed := srtpOfferSDP("RTP/AVP", "", nil, "a=rtcp-mux")
	plain := srtpOfferSDP("RTP/AVP", "", nil)

	l := newAnswerLeg(t, muxed)
	if _, err := l.negotiateInboundAnswer(codec.CodecUnknown); err != nil {
		t.Fatal(err)
	}
	if !l.prim.rtpSess.RTCPMuxed() {
		t.Error("an offer with a=rtcp-mux, answered with it, must mux RTCP")
	}

	// A re-offer without it moves RTCP off the RTP port.
	l.ApplyRemoteOffer([]byte(plain))
	if l.prim.rtpSess.RTCPMuxed() {
		t.Error("a re-offer without a=rtcp-mux left RTCP muxed")
	}
	// Our own re-offer then omits a=rtcp-mux, so an answer carrying it
	// still does not mux.
	l.applyAnswer([]byte(muxed))
	if l.prim.rtpSess.RTCPMuxed() {
		t.Error("an answer may not turn on a=rtcp-mux our offer left out")
	}

	l = newAnswerLeg(t, plain)
	if _, err := l.negotiateInboundAnswer(codec.CodecUnknown); err != nil {
		t.Fatal(err)
	}
	if l.prim.rtpSess.RTCPMuxed() {
		t.Error("an offer without a=rtcp-mux must not mux RTCP")
	}
}

func TestSetPrimaryRTCPMux(t *testing.T) {
	sess, err := sipmod.NewRTPSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	for _, tc := range []struct {
		sdp  string
		want bool
	}{
		{srtpOfferSDP("RTP/AVP", "", nil, "a=rtcp-mux"), true},
		{srtpOfferSDP("RTP/AVP", "", nil), false},
	} {
		remote, err := sipmod.ParseSDP([]byte(tc.sdp))
		if err != nil {
			t.Fatal(err)
		}
		setPrimaryRTCPMux(sess, remote)
		if sess.RTCPMuxed() != tc.want {
			t.Errorf("answer %q: muxed = %v", tc.sdp, !tc.want)
		}
	}
	setPrimaryRTCPMux(nil, nil)
}
