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
	}, codec.CodecPCMU, 0)
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
