package sip

import (
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/codec"
)

func mustParse(t *testing.T, lines ...string) *SDPMedia {
	t.Helper()
	m, err := ParseSDP(sdpLines(lines...))
	if err != nil {
		t.Fatalf("ParseSDP: %v", err)
	}
	return m
}

func twoAudioOffer(t *testing.T) *SDPMedia {
	return mustParse(t,
		"v=0",
		"o=- 1 0 IN IP4 192.0.2.1",
		"s=-",
		"c=IN IP4 192.0.2.1",
		"t=0 0",
		"m=audio 40000 RTP/AVP 0",
		"a=rtpmap:0 PCMU/8000",
		"a=sendrecv",
		"a=mid:orig",
		"a=lang:en",
		"m=audio 40002 RTP/AVP 0",
		"a=rtpmap:0 PCMU/8000",
		"a=sendonly",
		"a=mid:xlat",
		"a=content:alt",
		"a=lang:es",
	)
}

func baseOpts() AnswerOptions {
	return AnswerOptions{
		SupportedCodecs: []codec.CodecType{codec.CodecPCMU, codec.CodecPCMA},
		TextMLineIndex:  -1,
	}
}

func TestPlanAnswer_AlwaysCoversEveryOfferedSection(t *testing.T) {
	offer := twoAudioOffer(t)
	opts := baseOpts()
	opts.MultiStream = true
	opts.MaxStreams = 4

	plans := PlanAnswer(offer, opts)
	if len(plans) != len(offer.MLines) {
		t.Fatalf("plan count = %d, want %d — RFC 3264 §6 requires one answer section per offered section",
			len(plans), len(offer.MLines))
	}
	for i, p := range plans {
		if p.Index != i {
			t.Errorf("plan[%d].Index = %d, want %d (order must be preserved)", i, p.Index, i)
		}
	}
}

func TestPlanAnswer_MultiStreamDisabledRejectsExtras(t *testing.T) {
	plans := PlanAnswer(twoAudioOffer(t), baseOpts())

	if plans[0].Action != SlotAccept {
		t.Errorf("first section = %v, want accept", plans[0].Action)
	}
	if plans[1].Action != SlotReject {
		t.Fatalf("second section = %v, want reject when multi-stream is off", plans[1].Action)
	}
	if plans[1].Reason != "multi_stream_disabled" {
		t.Errorf("reason = %q, want multi_stream_disabled", plans[1].Reason)
	}
}

func TestPlanAnswer_MultiStreamAcceptsBoth(t *testing.T) {
	opts := baseOpts()
	opts.MultiStream = true
	opts.MaxStreams = 4

	plans := PlanAnswer(twoAudioOffer(t), opts)
	if plans[0].Action != SlotAccept || plans[1].Action != SlotAccept {
		t.Fatalf("actions = %v/%v, want both accepted", plans[0].Action, plans[1].Action)
	}
	// The offered sendonly stream must be answered recvonly (RFC 3264 §6.1).
	if plans[1].Direction != DirRecvOnly {
		t.Errorf("second direction = %q, want recvonly", plans[1].Direction)
	}
	if plans[0].Direction != DirSendRecv {
		t.Errorf("first direction = %q, want sendrecv", plans[0].Direction)
	}
	// Identity attributes must be echoed so the peer can correlate.
	if plans[1].MID != "xlat" || plans[1].Content != "alt" || plans[1].Lang != "es" {
		t.Errorf("second identity = mid %q content %q lang %q", plans[1].MID, plans[1].Content, plans[1].Lang)
	}
}

func TestPlanAnswer_MaxStreamsCap(t *testing.T) {
	opts := baseOpts()
	opts.MultiStream = true
	opts.MaxStreams = 1

	plans := PlanAnswer(twoAudioOffer(t), opts)
	if plans[1].Action != SlotReject || plans[1].Reason != "max_streams" {
		t.Errorf("second section = %v (%q), want reject/max_streams", plans[1].Action, plans[1].Reason)
	}
}

func TestPlanAnswer_EchoesOfferDisabledSection(t *testing.T) {
	offer := mustParse(t,
		"v=0",
		"o=- 1 0 IN IP4 192.0.2.1",
		"s=-",
		"c=IN IP4 192.0.2.1",
		"t=0 0",
		"m=audio 40000 RTP/AVP 0",
		"a=rtpmap:0 PCMU/8000",
		"m=audio 0 RTP/AVP 0",
		"a=rtpmap:0 PCMU/8000",
	)
	opts := baseOpts()
	opts.MultiStream = true
	opts.MaxStreams = 4

	plans := PlanAnswer(offer, opts)
	if plans[1].Action != SlotReject || plans[1].Reason != "offer_disabled" {
		t.Errorf("disabled section = %v (%q), want reject/offer_disabled", plans[1].Action, plans[1].Reason)
	}
	// A tombstoned slot must not consume the stream budget.
	if plans[0].Action != SlotAccept {
		t.Error("the live section must still be accepted")
	}
}

func TestPlanAnswer_NoCommonCodecRejectsOnlyThatSection(t *testing.T) {
	offer := mustParse(t,
		"v=0",
		"o=- 1 0 IN IP4 192.0.2.1",
		"s=-",
		"c=IN IP4 192.0.2.1",
		"t=0 0",
		"m=audio 40000 RTP/AVP 0",
		"a=rtpmap:0 PCMU/8000",
		"m=audio 40002 RTP/AVP 9",
		"a=rtpmap:9 G722/8000",
	)
	opts := baseOpts() // PCMU/PCMA only — G.722 is not supported here
	opts.MultiStream = true
	opts.MaxStreams = 4

	plans := PlanAnswer(offer, opts)
	if plans[0].Action != SlotAccept {
		t.Error("the negotiable section must be accepted")
	}
	if plans[1].Action != SlotReject || plans[1].Reason != "no_common_codec" {
		t.Errorf("undecodable section = %v (%q), want reject/no_common_codec", plans[1].Action, plans[1].Reason)
	}
}

func TestPlanAnswer_VideoOmittedUnlessStrict(t *testing.T) {
	offer := mustParse(t,
		"v=0",
		"o=- 1 0 IN IP4 192.0.2.1",
		"s=-",
		"c=IN IP4 192.0.2.1",
		"t=0 0",
		"m=audio 40000 RTP/AVP 0",
		"a=rtpmap:0 PCMU/8000",
		"m=video 40004 RTP/AVP 98",
		"a=rtpmap:98 H264/90000",
	)

	// Legacy shape: the video section is dropped from the answer entirely,
	// which is the RFC 3264 §6 violation the strict flag exists to fix.
	plans := PlanAnswer(offer, baseOpts())
	if plans[1].Action != SlotOmit {
		t.Errorf("video action = %v, want omit with strict off", plans[1].Action)
	}

	opts := baseOpts()
	opts.StrictMLines = true
	strict := PlanAnswer(offer, opts)
	if strict[1].Action != SlotReject || strict[1].Reason != "unsupported_media" {
		t.Errorf("video action = %v (%q), want reject/unsupported_media with strict on",
			strict[1].Action, strict[1].Reason)
	}
	if strict[1].Media != "video" {
		t.Errorf("Media = %q, want video preserved for the rejection", strict[1].Media)
	}
}

func TestPlanAnswer_TextSectionSkipped(t *testing.T) {
	offer := mustParse(t,
		"v=0",
		"o=- 1 0 IN IP4 192.0.2.1",
		"s=-",
		"c=IN IP4 192.0.2.1",
		"t=0 0",
		"m=audio 40000 RTP/AVP 0",
		"a=rtpmap:0 PCMU/8000",
		"m=text 40006 RTP/AVP 99",
		"a=rtpmap:99 t140/1000",
	)
	opts := baseOpts()
	opts.StrictMLines = true
	opts.TextMLineIndex = 1

	plans := PlanAnswer(offer, opts)
	if plans[1].Action != SlotSkip {
		t.Errorf("text action = %v, want skip — the RTT path renders that section", plans[1].Action)
	}
}

func TestPlanAnswer_PreferredCodecAppliesToFirstStreamOnly(t *testing.T) {
	offer := mustParse(t,
		"v=0",
		"o=- 1 0 IN IP4 192.0.2.1",
		"s=-",
		"c=IN IP4 192.0.2.1",
		"t=0 0",
		"m=audio 40000 RTP/AVP 0 8",
		"a=rtpmap:0 PCMU/8000",
		"a=rtpmap:8 PCMA/8000",
		"m=audio 40002 RTP/AVP 0 8",
		"a=rtpmap:0 PCMU/8000",
		"a=rtpmap:8 PCMA/8000",
	)
	opts := baseOpts()
	opts.MultiStream = true
	opts.MaxStreams = 4
	opts.Preferred = codec.CodecPCMA

	plans := PlanAnswer(offer, opts)
	if plans[0].Codec != codec.CodecPCMA {
		t.Errorf("first codec = %v, want the preferred PCMA", plans[0].Codec)
	}
	// Later streams fall back to plain offer-order preference.
	if plans[1].Codec != codec.CodecPCMU {
		t.Errorf("second codec = %v, want PCMU from offer order", plans[1].Codec)
	}
}

func TestPlanAnswer_NilOffer(t *testing.T) {
	if plans := PlanAnswer(nil, baseOpts()); plans != nil {
		t.Errorf("PlanAnswer(nil) = %v, want nil", plans)
	}
}

func TestAcceptedAudio(t *testing.T) {
	opts := baseOpts()
	opts.MultiStream = true
	opts.MaxStreams = 4
	got := AcceptedAudio(PlanAnswer(twoAudioOffer(t), opts))
	if len(got) != 2 {
		t.Fatalf("AcceptedAudio len = %d, want 2", len(got))
	}
	if got := AcceptedAudio(PlanAnswer(twoAudioOffer(t), baseOpts())); len(got) != 1 {
		t.Errorf("AcceptedAudio len = %d, want 1 with multi-stream off", len(got))
	}
}
