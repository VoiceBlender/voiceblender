package sip

import (
	"strings"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/codec"
)

func textCfg(extra func(*SDPConfig)) SDPConfig {
	cfg := SDPConfig{
		LocalIP: "192.0.2.10", RTPPort: 40000, Codecs: []codec.CodecType{codec.CodecPCMU},
		TextRTPPort: 40002, TextT140PT: 99, TextREDPT: 98, RTTRedundancy: 2,
	}
	if extra != nil {
		extra(&cfg)
	}
	return cfg
}

func TestTextSection_SRTP(t *testing.T) {
	offer, err := NewOfferCrypto()
	if err != nil {
		t.Fatal(err)
	}
	sdp := string(GenerateOffer(textCfg(func(c *SDPConfig) { c.TextCrypto = offer })))
	text := sdp[strings.Index(sdp, "m=text"):]
	if !strings.HasPrefix(text, "m=text 40002 RTP/SAVP 98 99\r\n") {
		t.Errorf("text section is not RTP/SAVP:\n%s", text)
	}
	for _, c := range offer {
		if !strings.Contains(text, "a=crypto:"+c.AttrValue()+"\r\n") {
			t.Errorf("text section lacks a=crypto:%s", c.AttrValue())
		}
	}
	if strings.Contains(text, "rtcp-mux") {
		t.Error("our text offers never carry a=rtcp-mux")
	}

	m, err := ParseSDP([]byte(sdp))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(m.Text.Proto, "/") != "RTP/SAVP" || len(m.Text.Crypto) != 2 || !m.Text.Crypto[1].SameKey(&offer[1]) || m.Text.RTCPMux {
		t.Errorf("parsed text = %+v", m.Text)
	}

	// An answer echoes the profile and, when offered, a=rtcp-mux.
	local := SDESCrypto{Tag: 2, Suite: SuiteAES128CMSHA1_32, Key: key30}
	ans := string(GenerateAnswer(textCfg(func(c *SDPConfig) {
		c.TextCrypto, c.TextProto, c.TextRTCPMux = []SDESCrypto{local}, []string{"RTP", "SAVPF"}, true
	}), codec.CodecPCMU, 0, false))
	text = ans[strings.Index(ans, "m=text"):]
	for _, want := range []string{"m=text 40002 RTP/SAVPF 98 99\r\n", "a=rtcp-mux\r\n", "a=crypto:2 AES_CM_128_HMAC_SHA1_32 " + inline(key30) + "\r\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("answer text section lacks %q:\n%s", want, text)
		}
	}
	m, _ = ParseSDP([]byte(ans))
	if !m.Text.RTCPMux {
		t.Error("a=rtcp-mux on m=text not parsed")
	}

	// A rejection repeats the offered profile.
	rej := string(GenerateAnswer(SDPConfig{LocalIP: "192.0.2.10", RTPPort: 40000, TextProto: []string{"RTP", "SAVP"}}, codec.CodecPCMU, 0, true))
	if !strings.Contains(rej, "m=text 0 RTP/SAVP 0\r\n") {
		t.Errorf("rejected SRTP text section:\n%s", rej)
	}
}

func TestKeyAnswerText(t *testing.T) {
	offered := []SDESCrypto{{Tag: 1, Suite: SuiteAES128CMSHA1_80, Key: key30}}
	peer := "a=crypto:1 AES_CM_128_HMAC_SHA1_80 " + inline([]byte("tsrqponmlkjihgfedcba9876543210"))
	answer := func(proto string, attrs ...string) *SDPTextMedia {
		lines := append([]string{"v=0", "o=- 1 0 IN IP4 192.0.2.1", "s=-", "c=IN IP4 192.0.2.1", "t=0 0",
			"m=audio 40000 RTP/AVP 0", "m=text 40002 " + proto + " 99", "a=rtpmap:99 t140/1000"}, attrs...)
		return mustParse(t, lines...).Text
	}

	sess := mustSession(t)
	if err := keyAnswerText(sess, offered, answer("RTP/SAVP", peer)); err != nil {
		t.Fatal(err)
	}
	if c := sess.LocalCrypto(); c == nil || !c.SameKey(&offered[0]) {
		t.Errorf("text session not keyed with the accepted offer: %+v", c)
	}
	for name, a := range map[string]*SDPTextMedia{
		"plain answer":   answer("RTP/AVP"),
		"SAVP no crypto": answer("RTP/SAVP"),
		"unknown tag":    answer("RTP/SAVP", strings.Replace(peer, "crypto:1", "crypto:3", 1)),
	} {
		if err := keyAnswerText(mustSession(t), offered, a); err == nil {
			t.Errorf("%s: should fail the call", name)
		}
	}
	if err := keyAnswerText(mustSession(t), nil, answer("RTP/AVP")); err != nil {
		t.Errorf("plain text offer: %v", err)
	}
}
