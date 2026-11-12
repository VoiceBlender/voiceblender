package leg

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
)

var (
	peerKeyA = []byte("0123456789abcdefghijklmnopqrst")
	peerKeyB = []byte("tsrqponmlkjihgfedcba9876543210")
)

func srtpOfferSDP(proto, suite string, key []byte, extra ...string) string {
	lines := []string{
		"v=0",
		"o=- 1 0 IN IP4 192.0.2.9",
		"s=-",
		"c=IN IP4 192.0.2.9",
		"t=0 0",
		"m=audio 40000 " + proto + " 0 101",
		"a=rtpmap:0 PCMU/8000",
		"a=rtpmap:101 telephone-event/8000",
		"a=sendrecv",
	}
	if suite != "" {
		lines = append(lines, "a=crypto:1 "+suite+" inline:"+base64.StdEncoding.EncodeToString(key))
	}
	return strings.Join(append(lines, extra...), "\r\n") + "\r\n"
}

func cryptoLines(sdp []byte) []string {
	var out []string
	for _, line := range strings.Split(string(sdp), "\r\n") {
		if strings.HasPrefix(line, "a=crypto:") {
			out = append(out, line)
		}
	}
	return out
}

func TestNegotiateInboundAnswer_SDES(t *testing.T) {
	l := newAnswerLeg(t, srtpOfferSDP("RTP/SAVP", sipmod.SuiteAES128CMSHA1_80, peerKeyA))

	answer, err := l.negotiateInboundAnswer(codec.CodecUnknown)
	if err != nil {
		t.Fatalf("negotiateInboundAnswer: %v", err)
	}
	if !strings.Contains(string(answer), " RTP/SAVP 0 101\r\n") {
		t.Errorf("answer is not RTP/SAVP:\n%s", answer)
	}
	lines := cryptoLines(answer)
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "a=crypto:1 AES_CM_128_HMAC_SHA1_80 inline:") {
		t.Fatalf("answer crypto = %q", lines)
	}
	local := l.prim.rtpSess.LocalCrypto()
	if local == nil {
		t.Fatal("primary session is not keyed")
	}
	if string(local.Key) == string(peerKeyA) {
		t.Error("our answer must carry our own key, not echo the peer's")
	}
	if !strings.Contains(lines[0], base64.StdEncoding.EncodeToString(local.Key)) {
		t.Error("the answered key is not the one the session encrypts with")
	}
}

func TestNegotiateInboundAnswer_EchoesSAVPF(t *testing.T) {
	l := newAnswerLeg(t, srtpOfferSDP("RTP/SAVPF", sipmod.SuiteAES128CMSHA1_32, peerKeyA))
	answer, err := l.negotiateInboundAnswer(codec.CodecUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(answer), " RTP/SAVPF 0 101\r\n") {
		t.Errorf("the offered profile must be echoed:\n%s", answer)
	}
	// Later offers on the dialog keep the profile.
	if got := string(l.reInviteSDP(sipmod.DirSendOnly)); !strings.Contains(got, " RTP/SAVPF ") || len(cryptoLines([]byte(got))) != 1 {
		t.Errorf("hold re-INVITE lost SRTP:\n%s", got)
	}
}

func TestNegotiateInboundAnswer_SRTPPolicy(t *testing.T) {
	plain := srtpOfferSDP("RTP/AVP", "", nil)
	sdes := srtpOfferSDP("RTP/SAVP", sipmod.SuiteAES128CMSHA1_80, peerKeyA)

	l := newAnswerLeg(t, plain)
	l.srtpMode = sipmod.SRTPRequired
	if _, err := l.negotiateInboundAnswer(codec.CodecUnknown); err == nil {
		t.Error("required: a plain offer must not be answered")
	}

	l = newAnswerLeg(t, sdes)
	l.srtpMode = sipmod.SRTPDisabled
	if _, err := l.negotiateInboundAnswer(codec.CodecUnknown); err == nil {
		t.Error("disabled: an SRTP offer must not be answered")
	}

	l = newAnswerLeg(t, plain)
	answer, err := l.negotiateInboundAnswer(codec.CodecUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(answer), "SAVP") || len(cryptoLines(answer)) != 0 || l.prim.rtpSess.SRTPEnabled() {
		t.Errorf("optional: a plain offer must stay plain:\n%s", answer)
	}
}

func TestApplyRemoteOffer_SRTPRekey(t *testing.T) {
	l := newAnswerLeg(t, srtpOfferSDP("RTP/SAVP", sipmod.SuiteAES128CMSHA1_80, peerKeyA))
	if _, err := l.negotiateInboundAnswer(codec.CodecUnknown); err != nil {
		t.Fatal(err)
	}
	first := l.prim.rtpSess.LocalCrypto()

	// The same suite with a new peer key: our key stays.
	answer, _, _ := l.ApplyRemoteOffer([]byte(srtpOfferSDP("RTP/SAVP", sipmod.SuiteAES128CMSHA1_80, peerKeyB)))
	if got := l.prim.rtpSess.LocalCrypto(); !got.SameKey(first) {
		t.Error("a re-offer keeping the suite must not change our key")
	}
	if lines := cryptoLines(answer); len(lines) != 1 || !strings.Contains(lines[0], base64.StdEncoding.EncodeToString(first.Key)) {
		t.Errorf("re-offer answer crypto = %q", lines)
	}

	// A new suite needs a key of that suite.
	l.ApplyRemoteOffer([]byte(srtpOfferSDP("RTP/SAVP", sipmod.SuiteAES128CMSHA1_32, peerKeyB)))
	if got := l.prim.rtpSess.LocalCrypto(); got == nil || got.Suite != sipmod.SuiteAES128CMSHA1_32 {
		t.Errorf("after a suite change LocalCrypto = %+v", got)
	}

	// Optional mode follows a peer that drops back to plain RTP.
	answer, _, _ = l.ApplyRemoteOffer([]byte(srtpOfferSDP("RTP/AVP", "", nil)))
	if l.prim.rtpSess.SRTPEnabled() || strings.Contains(string(answer), "SAVP") {
		t.Errorf("plain re-offer left SRTP on:\n%s", answer)
	}
}

// TestApplyRemoteOffer_RejectsUnacceptable covers a re-offer the policy
// refuses: it must come back as ErrNotAcceptable and leave the media alone.
func TestApplyRemoteOffer_RejectsUnacceptable(t *testing.T) {
	l := newAnswerLeg(t, srtpOfferSDP("RTP/SAVP", sipmod.SuiteAES128CMSHA1_80, peerKeyA))
	l.srtpMode = sipmod.SRTPRequired
	if _, err := l.negotiateInboundAnswer(codec.CodecUnknown); err != nil {
		t.Fatal(err)
	}
	local, remote := l.prim.rtpSess.LocalCrypto(), l.prim.rtpSess.RemoteAddr().String()

	plain := strings.Replace(srtpOfferSDP("RTP/AVP", "", nil, "a=sendonly"), "a=sendrecv\r\n", "", 1)
	plain = strings.Replace(plain, "m=audio 40000", "m=audio 40100", 1)
	answer, direction, err := l.ApplyRemoteOffer([]byte(plain))
	if !errors.Is(err, sipmod.ErrNotAcceptable) || answer != nil || direction != "" {
		t.Fatalf("plain re-offer under required: answer %q, direction %q, err %v", answer, direction, err)
	}
	if got := l.prim.rtpSess.LocalCrypto(); got == nil || !got.SameKey(local) {
		t.Error("a rejected re-offer changed the SRTP key")
	}
	if got := l.prim.rtpSess.RemoteAddr().String(); got != remote {
		t.Errorf("a rejected re-offer moved media from %s to %s", remote, got)
	}
}

func TestApplyAnswer_SRTPPeerRekeys(t *testing.T) {
	l := newReofferLeg(t)
	local := sipmod.SDESCrypto{Tag: 1, Suite: sipmod.SuiteAES128CMSHA1_80, Key: []byte("abcdefghijklmnopqrstuvwxyz0123")}
	peer := sipmod.SDESCrypto{Tag: 1, Suite: sipmod.SuiteAES128CMSHA1_80, Key: peerKeyA}
	if err := l.prim.rtpSess.SetSRTP(&local, &peer); err != nil {
		t.Fatal(err)
	}

	offer := l.reInviteSDP(sipmod.DirSendOnly)
	if !strings.Contains(string(offer), " RTP/SAVP ") || len(cryptoLines(offer)) != 1 {
		t.Fatalf("hold offer lost SRTP:\n%s", offer)
	}

	// The answer to our hold carries a new peer key under the same tag.
	l.applyAnswer([]byte(srtpOfferSDP("RTP/SAVP", sipmod.SuiteAES128CMSHA1_80, peerKeyB)))
	if got := l.prim.rtpSess.LocalCrypto(); !got.SameKey(&local) {
		t.Error("our key must not change when only the peer rekeys")
	}
	if !l.prim.rtpSess.SRTPEnabled() {
		t.Error("SRTP dropped after the peer rekeyed")
	}
}

func TestOfferedTextMLine_SRTP(t *testing.T) {
	text := func(proto string, attrs ...string) *sipmod.SDPMedia {
		m, err := sipmod.ParseSDP([]byte(srtpOfferSDP("RTP/AVP", "", nil,
			append([]string{"m=text 40002 " + proto + " 99", "a=rtpmap:99 t140/1000"}, attrs...)...)))
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	key := "a=crypto:1 " + sipmod.SuiteAES128CMSHA1_80 + " inline:" + base64.StdEncoding.EncodeToString(peerKeyA)
	for _, tc := range []struct {
		name string
		m    *sipmod.SDPMedia
		mode sipmod.SRTPMode
		want int
	}{
		{"plain under optional", text("RTP/AVP"), sipmod.SRTPOptional, 1},
		{"SDES under optional", text("RTP/SAVP", key), sipmod.SRTPOptional, 1},
		{"SDES under required", text("RTP/SAVP", key), sipmod.SRTPRequired, 1},
		{"SAVP without a key", text("RTP/SAVP"), sipmod.SRTPOptional, -1},
		{"plain under required", text("RTP/AVP"), sipmod.SRTPRequired, -1},
		{"SDES under disabled", text("RTP/SAVP", key), sipmod.SRTPDisabled, -1},
	} {
		if got := offeredTextMLine(tc.m, tc.mode); got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, got, tc.want)
		}
	}
}

func textOfferSDP(textProto string, textAttrs ...string) string {
	return srtpOfferSDP("RTP/SAVP", sipmod.SuiteAES128CMSHA1_80, peerKeyA, append([]string{
		"m=text 40002 " + textProto + " 98 99",
		"a=rtpmap:98 red/1000",
		"a=rtpmap:99 t140/1000",
		"a=fmtp:98 99/99/99",
		"a=sendrecv",
	}, textAttrs...)...)
}

func textCryptoAttr(key []byte) string {
	return "a=crypto:1 " + sipmod.SuiteAES128CMSHA1_80 + " inline:" + base64.StdEncoding.EncodeToString(key)
}

func textSection(sdp []byte) string {
	s := string(sdp)
	if i := strings.Index(s, "m=text"); i >= 0 {
		return s[i:]
	}
	return ""
}

func newTextAnswerLeg(t *testing.T, offer string) *SIPLeg {
	l := newAnswerLeg(t, offer)
	t.Cleanup(func() {
		if l.textRtpSess != nil {
			l.textRtpSess.Close()
		}
	})
	return l
}

func TestNegotiateInboundAnswer_SRTPText(t *testing.T) {
	l := newTextAnswerLeg(t, textOfferSDP("RTP/SAVP", "a=rtcp-mux", textCryptoAttr(peerKeyB)))
	answer, err := l.negotiateInboundAnswer(codec.CodecUnknown)
	if err != nil {
		t.Fatal(err)
	}
	text := textSection(answer)
	if !strings.HasPrefix(text, "m=text ") || !strings.Contains(text, " RTP/SAVP 98 99\r\n") || !strings.Contains(text, "a=rtcp-mux\r\n") {
		t.Fatalf("text answer:\n%s", text)
	}
	rs := l.textRtpSess
	local := rs.LocalCrypto()
	if local == nil || !rs.RTCPMuxed() {
		t.Fatalf("text session keyed=%v muxed=%v", local != nil, rs.RTCPMuxed())
	}
	if lines := cryptoLines([]byte(text)); len(lines) != 1 || !strings.Contains(lines[0], base64.StdEncoding.EncodeToString(local.Key)) {
		t.Errorf("text answer crypto = %q", lines)
	}
	if local.SameKey(l.prim.rtpSess.LocalCrypto()) {
		t.Error("text and audio must not share a key")
	}

	// Our hold re-offer keeps the text section encrypted and muxed.
	if hold := textSection(l.reInviteSDP(sipmod.DirSendOnly)); !strings.Contains(hold, " RTP/SAVP ") || len(cryptoLines([]byte(hold))) != 1 || !strings.Contains(hold, "a=rtcp-mux") {
		t.Errorf("hold re-INVITE text section:\n%s", hold)
	}

	// The peer re-keys under the same suite: our key stays.
	reAnswer, _, _ := l.ApplyRemoteOffer([]byte(textOfferSDP("RTP/SAVP", textCryptoAttr(peerKeyA))))
	if got := rs.LocalCrypto(); !got.SameKey(local) {
		t.Error("a re-offer keeping the suite must not change our text key")
	}
	if text := textSection(reAnswer); strings.Contains(text, "rtcp-mux") || rs.RTCPMuxed() {
		t.Errorf("a re-offer without a=rtcp-mux left the text section muxed:\n%s", text)
	}

	// Our re-offer's answer re-keys the peer's side.
	l.applyAnswer([]byte(textOfferSDP("RTP/SAVP", textCryptoAttr(peerKeyB))))
	if !rs.SRTPEnabled() || !rs.LocalCrypto().SameKey(local) {
		t.Error("text SRTP lost or our key changed on the answer to our re-offer")
	}

	// Optional mode follows the peer back to plain text.
	reAnswer, _, _ = l.ApplyRemoteOffer([]byte(textOfferSDP("RTP/AVP")))
	if text := textSection(reAnswer); rs.SRTPEnabled() || !strings.Contains(text, " RTP/AVP 98 99") || strings.Contains(text, "a=crypto") {
		t.Errorf("plain text re-offer left SRTP on:\n%s", text)
	}
}

func TestNegotiateInboundAnswer_TextPolicy(t *testing.T) {
	// Optional: the text section is judged on its own, like an audio section.
	l := newTextAnswerLeg(t, textOfferSDP("RTP/AVP"))
	answer, err := l.negotiateInboundAnswer(codec.CodecUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if text := textSection(answer); !strings.Contains(text, " RTP/AVP 98 99") || strings.Contains(text, "crypto") || l.textRtpSess.SRTPEnabled() {
		t.Errorf("optional: plain text must be answered plain:\n%s", text)
	}

	// Required: plain text is not carried.
	l = newTextAnswerLeg(t, textOfferSDP("RTP/AVP"))
	l.srtpMode = sipmod.SRTPRequired
	if answer, err = l.negotiateInboundAnswer(codec.CodecUnknown); err != nil {
		t.Fatal(err)
	}
	if l.textRtpSess != nil || strings.Contains(textSection(answer), "m=text 4") {
		t.Errorf("required: plain text was accepted:\n%s", answer)
	}
}

func TestSetSRTPMode(t *testing.T) {
	l := newAnswerLeg(t, srtpOfferSDP("RTP/AVP", "", nil))
	l.SetSRTPMode(sipmod.SRTPRequired)
	l.SetSRTPMode("")
	if l.srtpMode != sipmod.SRTPRequired {
		t.Fatalf("srtpMode = %q; an empty override must keep the current policy", l.srtpMode)
	}
	if _, err := l.negotiateInboundAnswer(codec.CodecUnknown); err == nil {
		t.Error("a trunk's required policy must refuse a plain offer")
	}
}

func TestStreams_ReportSRTP(t *testing.T) {
	srtpFlag := func(l *SIPLeg) bool {
		t.Helper()
		streams := l.Streams()
		if len(streams) != 1 {
			t.Fatalf("streams = %+v, want one", streams)
		}
		return streams[0].SRTP
	}

	l := newAnswerLeg(t, srtpOfferSDP("RTP/AVP", "", nil))
	if _, err := l.negotiateInboundAnswer(codec.CodecUnknown); err != nil {
		t.Fatal(err)
	}
	if srtpFlag(l) {
		t.Error("a plain stream reports SRTP")
	}

	l = newAnswerLeg(t, srtpOfferSDP("RTP/SAVP", sipmod.SuiteAES128CMSHA1_80, peerKeyA))
	if _, err := l.negotiateInboundAnswer(codec.CodecUnknown); err != nil {
		t.Fatal(err)
	}
	if !srtpFlag(l) {
		t.Error("an SDES stream does not report SRTP")
	}

	// Optional mode follows a peer that drops back to plain RTP, and so does the view.
	if _, _, err := l.ApplyRemoteOffer([]byte(srtpOfferSDP("RTP/AVP", "", nil))); err != nil {
		t.Fatal(err)
	}
	if srtpFlag(l) {
		t.Error("the stream still reports SRTP after a plain re-offer")
	}
}
