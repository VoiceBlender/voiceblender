package leg

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
)

// newAnswerLeg builds an engine-less inbound leg carrying the given offer.
// Without an engine the media sockets take OS-assigned ports, which is all
// these tests need.
func newAnswerLeg(t *testing.T, offerSDP string) *SIPLeg {
	t.Helper()
	offer, err := sipmod.ParseSDP([]byte(offerSDP))
	if err != nil {
		t.Fatalf("ParseSDP: %v", err)
	}
	l := newTestSIPLeg(codec.CodecPCMU)
	l.log = slog.New(slog.DiscardHandler)
	l.localIP = "192.0.2.1"
	l.supportedCodecs = []codec.CodecType{codec.CodecPCMU, codec.CodecPCMA}
	l.inbound = &sipmod.InboundCall{RemoteSDP: offer}
	t.Cleanup(l.closeAllStreams)
	return l
}

const twoAudioOfferSDP = "v=0\r\n" +
	"o=- 1 0 IN IP4 192.0.2.9\r\n" +
	"s=-\r\n" +
	"c=IN IP4 192.0.2.9\r\n" +
	"t=0 0\r\n" +
	"m=audio 40000 RTP/AVP 0\r\n" +
	"a=rtpmap:0 PCMU/8000\r\n" +
	"a=sendrecv\r\n" +
	"a=mid:orig\r\n" +
	"a=lang:en\r\n" +
	"m=audio 40002 RTP/AVP 0\r\n" +
	"a=rtpmap:0 PCMU/8000\r\n" +
	"a=sendonly\r\n" +
	"a=mid:xlat\r\n" +
	"a=content:alt\r\n" +
	"a=lang:es\r\n"

func countMLines(sdp []byte) int {
	return strings.Count(string(sdp), "m=audio") + strings.Count(string(sdp), "m=video") +
		strings.Count(string(sdp), "m=text")
}

func TestNegotiateInboundAnswer_AcceptsEveryOfferedSection(t *testing.T) {
	l := newAnswerLeg(t, twoAudioOfferSDP)

	answer, err := l.negotiateInboundAnswer(codec.CodecUnknown)
	if err != nil {
		t.Fatalf("negotiateInboundAnswer: %v", err)
	}

	if got := countMLines(answer); got != 2 {
		t.Fatalf("answer m-line count = %d, want 2:\n%s", got, answer)
	}
	if strings.Contains(string(answer), "m=audio 0 RTP/AVP") {
		t.Errorf("no section should be rejected:\n%s", answer)
	}
	// The offered sendonly section must be answered recvonly.
	if !strings.Contains(string(answer), "a=recvonly") {
		t.Errorf("sendonly offer not mirrored to recvonly:\n%s", answer)
	}
	// Identity attributes must round-trip so the peer can correlate streams.
	for _, want := range []string{"a=mid:orig", "a=mid:xlat", "a=content:alt", "a=lang:es"} {
		if !strings.Contains(string(answer), want) {
			t.Errorf("answer missing %q:\n%s", want, answer)
		}
	}

	streams := l.audioStreams()
	if len(streams) != 2 {
		t.Fatalf("materialized %d streams, want 2", len(streams))
	}
	if streams[0].mid != "orig" || streams[1].mid != "xlat" {
		t.Errorf("stream mids = %q/%q, want orig/xlat", streams[0].mid, streams[1].mid)
	}
	// The recvonly stream must not transmit.
	if streams[1].sends() {
		t.Error("a recvonly stream must not run a writeLoop")
	}
	if streams[0].rtpSess.LocalPort() == streams[1].rtpSess.LocalPort() {
		t.Error("streams must bind distinct ports — shared transport is undefined without BUNDLE")
	}
	if l.mlines.Len() != 2 {
		t.Errorf("m-line table len = %d, want 2", l.mlines.Len())
	}
}

func TestNegotiateInboundAnswer_SingleStreamOfferUnchanged(t *testing.T) {
	l := newAnswerLeg(t, "v=0\r\n"+
		"o=- 1 0 IN IP4 192.0.2.9\r\n"+
		"s=-\r\n"+
		"c=IN IP4 192.0.2.9\r\n"+
		"t=0 0\r\n"+
		"m=audio 40000 RTP/AVP 0 101\r\n"+
		"a=rtpmap:0 PCMU/8000\r\n"+
		"a=rtpmap:101 telephone-event/8000\r\n")

	answer, err := l.negotiateInboundAnswer(codec.CodecUnknown)
	if err != nil {
		t.Fatalf("negotiateInboundAnswer: %v", err)
	}
	if got := countMLines(answer); got != 1 {
		t.Fatalf("answer m-line count = %d, want 1:\n%s", got, answer)
	}
	// No a=mid was offered, so none may appear — the classic single-stream
	// answer must stay byte-for-byte what deployed peers already negotiate.
	if strings.Contains(string(answer), "a=mid") {
		t.Errorf("unsolicited a=mid in the answer:\n%s", answer)
	}
	for _, want := range []string{"a=sendrecv", "a=ptime:20", "a=rtcp-mux", "a=rtpmap:0 PCMU/8000"} {
		if !strings.Contains(string(answer), want) {
			t.Errorf("answer missing %q:\n%s", want, answer)
		}
	}
}

func TestNegotiateInboundAnswer_NoCommonCodecFails(t *testing.T) {
	l := newAnswerLeg(t, "v=0\r\n"+
		"o=- 1 0 IN IP4 192.0.2.9\r\n"+
		"s=-\r\n"+
		"c=IN IP4 192.0.2.9\r\n"+
		"t=0 0\r\n"+
		"m=audio 40000 RTP/AVP 9\r\n"+
		"a=rtpmap:9 G722/8000\r\n")

	if _, err := l.negotiateInboundAnswer(codec.CodecUnknown); err == nil {
		t.Fatal("want an error when no audio section can be negotiated")
	}
}

func TestNegotiateInboundAnswer_RejectsVideoWhenStrict(t *testing.T) {
	l := newAnswerLeg(t, "v=0\r\n"+
		"o=- 1 0 IN IP4 192.0.2.9\r\n"+
		"s=-\r\n"+
		"c=IN IP4 192.0.2.9\r\n"+
		"t=0 0\r\n"+
		"m=audio 40000 RTP/AVP 0\r\n"+
		"a=rtpmap:0 PCMU/8000\r\n"+
		"m=video 40004 RTP/AVP 98\r\n"+
		"a=rtpmap:98 H264/90000\r\n")
	l.strictMLines = true

	answer, err := l.negotiateInboundAnswer(codec.CodecUnknown)
	if err != nil {
		t.Fatalf("negotiateInboundAnswer: %v", err)
	}
	// The offer had two sections, so the answer must too — this is the
	// RFC 3264 §6 violation the strict flag fixes.
	if got := countMLines(answer); got != 2 {
		t.Fatalf("answer m-line count = %d, want 2:\n%s", got, answer)
	}
}
