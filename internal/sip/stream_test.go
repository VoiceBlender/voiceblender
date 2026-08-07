package sip

import (
	"strings"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/codec"
)

func sdpLines(lines ...string) []byte {
	return []byte(strings.Join(lines, "\r\n") + "\r\n")
}

func TestParseSDP_MultipleAudioMLines(t *testing.T) {
	raw := sdpLines(
		"v=0",
		"o=- 1 0 IN IP4 192.0.2.1",
		"s=-",
		"c=IN IP4 192.0.2.1",
		"t=0 0",
		"m=audio 10000 RTP/AVP 0 101",
		"a=rtpmap:0 PCMU/8000",
		"a=rtpmap:101 telephone-event/8000",
		"a=sendrecv",
		"a=mid:orig",
		"m=audio 10002 RTP/AVP 8",
		"a=rtpmap:8 PCMA/8000",
		"a=sendonly",
		"a=mid:xlat",
	)

	m, err := ParseSDP(raw)
	if err != nil {
		t.Fatalf("ParseSDP: %v", err)
	}
	if len(m.Audio) != 2 {
		t.Fatalf("want 2 audio sections, got %d", len(m.Audio))
	}
	if len(m.MLines) != 2 {
		t.Fatalf("want 2 m-lines, got %d", len(m.MLines))
	}
	if m.PrimaryAudio != 0 {
		t.Errorf("PrimaryAudio = %d, want 0", m.PrimaryAudio)
	}

	// Legacy scalars must mirror the primary section exactly.
	if m.RemotePort != 10000 || m.Direction != DirSendRecv {
		t.Errorf("scalars = port %d dir %q, want 10000 sendrecv", m.RemotePort, m.Direction)
	}
	if len(m.Codecs) != 1 || m.Codecs[0] != codec.CodecPCMU {
		t.Errorf("scalar codecs = %v, want [PCMU]", m.Codecs)
	}

	second := m.Audio[1]
	if second.RemotePort != 10002 {
		t.Errorf("second port = %d, want 10002", second.RemotePort)
	}
	if second.Direction != DirSendOnly {
		t.Errorf("second direction = %q, want sendonly", second.Direction)
	}
	if second.MID != "xlat" {
		t.Errorf("second mid = %q, want xlat", second.MID)
	}
	if second.RemoteIP != "192.0.2.1" {
		t.Errorf("second IP = %q, want the session c= address", second.RemoteIP)
	}
	if len(second.Codecs) != 1 || second.Codecs[0] != codec.CodecPCMA {
		t.Errorf("second codecs = %v, want [PCMA]", second.Codecs)
	}
	if m.MLines[1].AudioIdx != 1 {
		t.Errorf("MLines[1].AudioIdx = %d, want 1", m.MLines[1].AudioIdx)
	}
}

func TestParseSDP_FirstAudioRejectedSecondActive(t *testing.T) {
	raw := sdpLines(
		"v=0",
		"o=- 1 0 IN IP4 192.0.2.1",
		"s=-",
		"c=IN IP4 192.0.2.1",
		"t=0 0",
		"m=audio 0 RTP/AVP 0",
		"a=rtpmap:0 PCMU/8000",
		"m=audio 10002 RTP/AVP 8",
		"a=rtpmap:8 PCMA/8000",
	)

	m, err := ParseSDP(raw)
	if err != nil {
		t.Fatalf("ParseSDP: %v", err)
	}
	if m.PrimaryAudio != 1 {
		t.Fatalf("PrimaryAudio = %d, want 1 (first section is rejected)", m.PrimaryAudio)
	}
	if m.RemotePort != 10002 {
		t.Errorf("RemotePort = %d, want 10002", m.RemotePort)
	}
	if m.Audio[0].RemotePort != 0 {
		t.Errorf("rejected section must be retained with port 0, got %d", m.Audio[0].RemotePort)
	}
}

func TestParseSDP_AllAudioRejectedErrors(t *testing.T) {
	raw := sdpLines(
		"v=0",
		"o=- 1 0 IN IP4 192.0.2.1",
		"s=-",
		"c=IN IP4 192.0.2.1",
		"t=0 0",
		"m=audio 0 RTP/AVP 0",
		"a=rtpmap:0 PCMU/8000",
	)

	if _, err := ParseSDP(raw); err == nil {
		t.Fatal("want an error when every audio section is rejected")
	}
}

func TestParseSDP_MidLabelContentLang(t *testing.T) {
	raw := sdpLines(
		"v=0",
		"o=- 1 0 IN IP4 192.0.2.1",
		"s=-",
		"c=IN IP4 192.0.2.1",
		"t=0 0",
		"m=audio 10000 RTP/AVP 0",
		"a=rtpmap:0 PCMU/8000",
		"a=mid:1",
		"a=label:2",
		"a=content:alt",
		"a=lang:es-ES",
		"a=rtcp-mux",
	)

	m, err := ParseSDP(raw)
	if err != nil {
		t.Fatalf("ParseSDP: %v", err)
	}
	a := m.Audio[0]
	if a.MID != "1" || a.Label != "2" || a.Content != "alt" || a.Lang != "es-ES" {
		t.Errorf("attributes = mid %q label %q content %q lang %q", a.MID, a.Label, a.Content, a.Lang)
	}
	if !a.RTCPMux {
		t.Error("want RTCPMux true")
	}
	if m.MLines[0].MID != "1" {
		t.Errorf("MLines[0].MID = %q, want 1", m.MLines[0].MID)
	}
}

func TestParseSDP_SessionLevelDirection(t *testing.T) {
	raw := sdpLines(
		"v=0",
		"o=- 1 0 IN IP4 192.0.2.1",
		"s=-",
		"c=IN IP4 192.0.2.1",
		"t=0 0",
		"a=inactive",
		"m=audio 10000 RTP/AVP 0",
		"a=rtpmap:0 PCMU/8000",
	)

	m, err := ParseSDP(raw)
	if err != nil {
		t.Fatalf("ParseSDP: %v", err)
	}
	if m.Direction != DirInactive {
		t.Errorf("Direction = %q, want inactive inherited from the session level", m.Direction)
	}
}

func TestParseSDP_MediaDirectionOverridesSession(t *testing.T) {
	raw := sdpLines(
		"v=0",
		"o=- 1 0 IN IP4 192.0.2.1",
		"s=-",
		"c=IN IP4 192.0.2.1",
		"t=0 0",
		"a=inactive",
		"m=audio 10000 RTP/AVP 0",
		"a=rtpmap:0 PCMU/8000",
		"a=sendrecv",
	)

	m, err := ParseSDP(raw)
	if err != nil {
		t.Fatalf("ParseSDP: %v", err)
	}
	if m.Direction != DirSendRecv {
		t.Errorf("Direction = %q, want the media-level sendrecv to win", m.Direction)
	}
}

func TestParseSDP_AbsentDirectionStaysEmpty(t *testing.T) {
	raw := sdpLines(
		"v=0",
		"o=- 1 0 IN IP4 192.0.2.1",
		"s=-",
		"c=IN IP4 192.0.2.1",
		"t=0 0",
		"m=audio 10000 RTP/AVP 0",
		"a=rtpmap:0 PCMU/8000",
	)

	m, err := ParseSDP(raw)
	if err != nil {
		t.Fatalf("ParseSDP: %v", err)
	}
	// Callers distinguish "absent" from an explicit sendrecv; only the
	// EffectiveDirection helper applies the RFC 8866 default.
	if m.Direction != "" {
		t.Errorf("Direction = %q, want empty when no attribute is present", m.Direction)
	}
	if got := m.Audio[0].EffectiveDirection(); got != DirSendRecv {
		t.Errorf("EffectiveDirection = %q, want sendrecv", got)
	}
}

func TestParseSDP_MLinesRecordsVideo(t *testing.T) {
	raw := sdpLines(
		"v=0",
		"o=- 1 0 IN IP4 192.0.2.1",
		"s=-",
		"c=IN IP4 192.0.2.1",
		"t=0 0",
		"m=audio 10000 RTP/AVP 0",
		"a=rtpmap:0 PCMU/8000",
		"m=video 10004 RTP/AVP 98",
		"a=rtpmap:98 H264/90000",
	)

	m, err := ParseSDP(raw)
	if err != nil {
		t.Fatalf("ParseSDP: %v", err)
	}
	if len(m.MLines) != 2 {
		t.Fatalf("want 2 m-lines recorded, got %d", len(m.MLines))
	}
	v := m.MLines[1]
	if v.Media != "video" || v.Port != 10004 || v.AudioIdx != -1 {
		t.Errorf("video m-line = %+v", v)
	}
	if strings.Join(v.Proto, "/") != "RTP/AVP" {
		t.Errorf("Proto = %v, want RTP/AVP so a rejection can echo it", v.Proto)
	}
	if len(m.Audio) != 1 {
		t.Errorf("want 1 audio section, got %d", len(m.Audio))
	}
}

func TestParseSDP_ScalarsMatchPrimaryStream(t *testing.T) {
	raw := sdpLines(
		"v=0",
		"o=- 1 0 IN IP4 192.0.2.1",
		"s=-",
		"c=IN IP4 192.0.2.1",
		"t=0 0",
		"m=audio 10000 RTP/AVP 96 101",
		"a=rtpmap:96 AMR-WB/16000/1",
		"a=fmtp:96 octet-align=1; mode-set=0,1,2",
		"a=rtpmap:101 telephone-event/16000",
		"a=ptime:20",
		"a=recvonly",
	)

	m, err := ParseSDP(raw)
	if err != nil {
		t.Fatalf("ParseSDP: %v", err)
	}
	p := m.PrimaryStream()
	if p == nil {
		t.Fatal("PrimaryStream is nil")
	}
	if m.RemotePort != p.RemotePort || m.RemoteIP != p.RemoteIP || m.Direction != p.Direction || m.Ptime != p.Ptime {
		t.Errorf("scalars diverged from the primary stream")
	}
	if m.CodecFmtp[codec.CodecAMRWB] != p.CodecFmtp[codec.CodecAMRWB] {
		t.Errorf("fmtp diverged: %q vs %q", m.CodecFmtp[codec.CodecAMRWB], p.CodecFmtp[codec.CodecAMRWB])
	}
	if pt, rate, ok := p.PreferredDTMFEvent(); !ok || pt != 101 || rate != 16000 {
		t.Errorf("PreferredDTMFEvent = %d/%d ok=%v, want 101/16000", pt, rate, ok)
	}
}

func TestAudioByMID(t *testing.T) {
	raw := sdpLines(
		"v=0",
		"o=- 1 0 IN IP4 192.0.2.1",
		"s=-",
		"c=IN IP4 192.0.2.1",
		"t=0 0",
		"m=audio 10000 RTP/AVP 0",
		"a=rtpmap:0 PCMU/8000",
		"a=mid:orig",
		"m=audio 10002 RTP/AVP 0",
		"a=rtpmap:0 PCMU/8000",
		"a=mid:xlat",
	)

	m, err := ParseSDP(raw)
	if err != nil {
		t.Fatalf("ParseSDP: %v", err)
	}
	got, ok := m.AudioByMID("xlat")
	if !ok || got.RemotePort != 10002 {
		t.Errorf("AudioByMID(xlat) = %+v ok=%v", got, ok)
	}
	if _, ok := m.AudioByMID("nope"); ok {
		t.Error("AudioByMID must report a miss for an unknown mid")
	}
	if _, ok := m.AudioByMID(""); ok {
		t.Error("AudioByMID must report a miss for an empty mid")
	}
}

func TestMirrorDirection(t *testing.T) {
	cases := map[string]string{
		DirSendOnly: DirRecvOnly,
		DirRecvOnly: DirSendOnly,
		DirInactive: DirInactive,
		DirSendRecv: DirSendRecv,
		"":          DirSendRecv,
	}
	for offered, want := range cases {
		if got := MirrorDirection(offered); got != want {
			t.Errorf("MirrorDirection(%q) = %q, want %q", offered, got, want)
		}
	}
}

func TestHoldDirection(t *testing.T) {
	cases := []struct {
		desired string
		held    bool
		want    string
	}{
		{DirSendRecv, false, DirSendRecv},
		{"", false, DirSendRecv},
		{DirSendOnly, false, DirSendOnly},
		{DirSendRecv, true, DirSendOnly},
		{DirSendOnly, true, DirSendOnly},
		{DirRecvOnly, true, DirInactive},
		{DirInactive, true, DirInactive},
	}
	for _, c := range cases {
		if got := HoldDirection(c.desired, c.held); got != c.want {
			t.Errorf("HoldDirection(%q, %v) = %q, want %q", c.desired, c.held, got, c.want)
		}
	}
}

func TestAudioStreamPayloadType(t *testing.T) {
	s := AudioStream{CodecPTs: map[codec.CodecType]uint8{codec.CodecAMRWB: 96}}
	if got := s.PayloadType(codec.CodecAMRWB); got != 96 {
		t.Errorf("PayloadType(AMR-WB) = %d, want the explicit 96", got)
	}
	if got := s.PayloadType(codec.CodecPCMU); got != codec.CodecPCMU.PayloadType() {
		t.Errorf("PayloadType(PCMU) = %d, want the static default", got)
	}
}
