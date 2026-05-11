package sip

import (
	"strings"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/codec"
)

func TestGenerateOffer_IPv4(t *testing.T) {
	sdp := GenerateOffer(SDPConfig{
		LocalIP: "127.0.0.1",
		RTPPort: 10000,
		Codecs:  []codec.CodecType{codec.CodecPCMU},
	})
	s := string(sdp)
	if !strings.Contains(s, "o=- ") || !strings.Contains(s, " IN IP4 127.0.0.1") {
		t.Errorf("offer missing IN IP4 origin:\n%s", s)
	}
	if !strings.Contains(s, "c=IN IP4 127.0.0.1") {
		t.Errorf("offer missing IN IP4 connection:\n%s", s)
	}
}

func TestGenerateOffer_IPv6(t *testing.T) {
	sdp := GenerateOffer(SDPConfig{
		LocalIP: "2001:db8::1",
		RTPPort: 10000,
		Codecs:  []codec.CodecType{codec.CodecPCMU},
	})
	s := string(sdp)
	if !strings.Contains(s, " IN IP6 2001:db8::1") {
		t.Errorf("offer missing IN IP6 origin:\n%s", s)
	}
	if !strings.Contains(s, "c=IN IP6 2001:db8::1") {
		t.Errorf("offer missing IN IP6 connection:\n%s", s)
	}
}

func TestGenerateAnswer_IPv6(t *testing.T) {
	sdp := GenerateAnswer(SDPConfig{
		LocalIP: "::1",
		RTPPort: 10002,
		Codecs:  []codec.CodecType{codec.CodecPCMU},
	}, codec.CodecPCMU, 0, false)
	if !strings.Contains(string(sdp), "c=IN IP6 ::1") {
		t.Errorf("answer missing IN IP6 connection:\n%s", sdp)
	}
}

func TestParseSDP_IPv6(t *testing.T) {
	raw := strings.Join([]string{
		"v=0",
		"o=- 1 0 IN IP6 2001:db8::1",
		"s=-",
		"c=IN IP6 2001:db8::1",
		"t=0 0",
		"m=audio 10000 RTP/AVP 0",
		"a=rtpmap:0 PCMU/8000",
		"a=ptime:20",
		"a=sendrecv",
		"",
	}, "\r\n")
	m, err := ParseSDP([]byte(raw))
	if err != nil {
		t.Fatalf("ParseSDP: %v", err)
	}
	if m.RemoteIP != "2001:db8::1" {
		t.Errorf("RemoteIP = %q, want 2001:db8::1", m.RemoteIP)
	}
	if m.AddressFamily != "IP6" {
		t.Errorf("AddressFamily = %q, want IP6", m.AddressFamily)
	}
	if m.RemotePort != 10000 {
		t.Errorf("RemotePort = %d, want 10000", m.RemotePort)
	}
}

func TestParseSDP_IPv4(t *testing.T) {
	raw := strings.Join([]string{
		"v=0",
		"o=- 1 0 IN IP4 192.0.2.1",
		"s=-",
		"c=IN IP4 192.0.2.1",
		"t=0 0",
		"m=audio 10000 RTP/AVP 0",
		"a=rtpmap:0 PCMU/8000",
		"a=ptime:20",
		"a=sendrecv",
		"",
	}, "\r\n")
	m, err := ParseSDP([]byte(raw))
	if err != nil {
		t.Fatalf("ParseSDP: %v", err)
	}
	if m.AddressFamily != "IP4" {
		t.Errorf("AddressFamily = %q, want IP4", m.AddressFamily)
	}
}

func TestParseSDP_CodecRates(t *testing.T) {
	raw := strings.Join([]string{
		"v=0",
		"o=- 1 0 IN IP4 192.0.2.1",
		"s=-",
		"c=IN IP4 192.0.2.1",
		"t=0 0",
		"m=audio 10000 RTP/AVP 111 0 8",
		"a=rtpmap:111 opus/48000/2",
		"a=rtpmap:0 PCMU/8000",
		"a=rtpmap:8 PCMA/8000",
		"",
	}, "\r\n")
	m, err := ParseSDP([]byte(raw))
	if err != nil {
		t.Fatalf("ParseSDP: %v", err)
	}
	want := map[codec.CodecType]int{
		codec.CodecOpus: 48000,
		codec.CodecPCMU: 8000,
		codec.CodecPCMA: 8000,
	}
	for c, r := range want {
		if got := m.CodecRates[c]; got != r {
			t.Errorf("CodecRates[%s] = %d, want %d", c, got, r)
		}
	}
	// Offer order must be preserved.
	if len(m.Codecs) != 3 || m.Codecs[0] != codec.CodecOpus || m.Codecs[1] != codec.CodecPCMU || m.Codecs[2] != codec.CodecPCMA {
		t.Errorf("Codecs = %v, want [opus PCMU PCMA]", m.Codecs)
	}
}

func TestGenerateOfferWithText(t *testing.T) {
	sdp := GenerateOffer(SDPConfig{
		LocalIP:       "127.0.0.1",
		RTPPort:       10000,
		Codecs:        []codec.CodecType{codec.CodecPCMU},
		TextRTPPort:   10010,
		TextT140PT:    99,
		TextREDPT:     98,
		RTTRedundancy: 2,
	})
	s := string(sdp)
	if !strings.Contains(s, "m=text 10010 RTP/AVP 98 99") {
		t.Errorf("offer missing m=text line:\n%s", s)
	}
	if !strings.Contains(s, "a=rtpmap:99 t140/1000") {
		t.Errorf("offer missing t140 rtpmap:\n%s", s)
	}
	if !strings.Contains(s, "a=rtpmap:98 red/1000") {
		t.Errorf("offer missing red rtpmap:\n%s", s)
	}
	if !strings.Contains(s, "a=fmtp:98 99/99/99") {
		t.Errorf("offer missing red fmtp with 3 generations:\n%s", s)
	}
}

func TestParseSDPWithText(t *testing.T) {
	raw := []byte("v=0\r\n" +
		"o=- 0 0 IN IP4 192.0.2.1\r\n" +
		"s=-\r\n" +
		"c=IN IP4 192.0.2.1\r\n" +
		"t=0 0\r\n" +
		"m=audio 5000 RTP/AVP 0\r\n" +
		"a=rtpmap:0 PCMU/8000\r\n" +
		"m=text 5004 RTP/AVP 98 99\r\n" +
		"a=rtpmap:98 red/1000\r\n" +
		"a=rtpmap:99 t140/1000\r\n" +
		"a=fmtp:98 99/99/99\r\n")
	m, err := ParseSDP(raw)
	if err != nil {
		t.Fatalf("ParseSDP: %v", err)
	}
	if m.Text == nil {
		t.Fatalf("expected Text section to be parsed")
	}
	if m.Text.RemotePort != 5004 || m.Text.RemoteIP != "192.0.2.1" {
		t.Errorf("text remote: got %s:%d", m.Text.RemoteIP, m.Text.RemotePort)
	}
	if m.Text.T140PT != 99 || m.Text.REDPT != 98 {
		t.Errorf("text PTs: got t140=%d red=%d", m.Text.T140PT, m.Text.REDPT)
	}
}

func TestParseSDPRejectedText(t *testing.T) {
	raw := []byte("v=0\r\n" +
		"o=- 0 0 IN IP4 192.0.2.1\r\n" +
		"s=-\r\n" +
		"c=IN IP4 192.0.2.1\r\n" +
		"t=0 0\r\n" +
		"m=audio 5000 RTP/AVP 0\r\n" +
		"a=rtpmap:0 PCMU/8000\r\n" +
		"m=text 0 RTP/AVP 0\r\n")
	m, err := ParseSDP(raw)
	if err != nil {
		t.Fatalf("ParseSDP: %v", err)
	}
	if m.Text != nil {
		t.Errorf("rejected text section (port=0) must leave Text nil; got %+v", m.Text)
	}
}

func TestGenerateAnswerRejected(t *testing.T) {
	sdp := GenerateAnswer(SDPConfig{
		LocalIP: "127.0.0.1",
		RTPPort: 10002,
		Codecs:  []codec.CodecType{codec.CodecPCMU},
	}, codec.CodecPCMU, 0, true)
	s := string(sdp)
	if !strings.Contains(s, "m=text 0 ") {
		t.Errorf("rejected text section missing port=0 m=text line:\n%s", s)
	}
}

func TestNegotiateCodecPreferred(t *testing.T) {
	remote := &SDPMedia{
		Codecs: []codec.CodecType{codec.CodecOpus, codec.CodecPCMU, codec.CodecPCMA},
		CodecPTs: map[codec.CodecType]uint8{
			codec.CodecOpus: 111,
			codec.CodecPCMU: 0,
			codec.CodecPCMA: 8,
		},
	}
	supported := []codec.CodecType{codec.CodecOpus, codec.CodecPCMU, codec.CodecPCMA}

	// Default (no preference) → first in offer.
	c, pt, ok := NegotiateCodecPreferred(remote, supported, codec.CodecUnknown)
	if !ok || c != codec.CodecOpus || pt != 111 {
		t.Errorf("default = (%s, %d, %v), want (opus, 111, true)", c, pt, ok)
	}

	// Preferred wins when offered + supported.
	c, pt, ok = NegotiateCodecPreferred(remote, supported, codec.CodecPCMU)
	if !ok || c != codec.CodecPCMU || pt != 0 {
		t.Errorf("prefer PCMU = (%s, %d, %v), want (PCMU, 0, true)", c, pt, ok)
	}

	// Preferred not offered → falls back to first match.
	remoteNoG722 := &SDPMedia{
		Codecs:   []codec.CodecType{codec.CodecPCMU},
		CodecPTs: map[codec.CodecType]uint8{codec.CodecPCMU: 0},
	}
	c, _, ok = NegotiateCodecPreferred(remoteNoG722, supported, codec.CodecG722)
	if !ok || c != codec.CodecPCMU {
		t.Errorf("prefer-not-offered = (%s, %v), want (PCMU, true)", c, ok)
	}

	// Preferred not in supported list → falls back.
	c, _, ok = NegotiateCodecPreferred(remote, []codec.CodecType{codec.CodecPCMA}, codec.CodecOpus)
	if !ok || c != codec.CodecPCMA {
		t.Errorf("prefer-not-supported = (%s, %v), want (PCMA, true)", c, ok)
	}
}
