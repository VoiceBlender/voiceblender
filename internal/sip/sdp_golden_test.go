package sip

import (
	"regexp"
	"strings"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/codec"
)

// originLine matches the o= line, whose session ID is random per generation.
var originLine = regexp.MustCompile(`(?m)^o=.*$`)

// normalizeSDP makes generated SDP comparable across runs by pinning the
// random session ID in the o= line.
func normalizeSDP(b []byte) string {
	return originLine.ReplaceAllString(strings.ReplaceAll(string(b), "\r\n", "\n"), "o=<origin>")
}

func assertSDP(t *testing.T, got []byte, want string) {
	t.Helper()
	if g := normalizeSDP(got); g != want {
		t.Errorf("SDP mismatch\n--- got ---\n%s\n--- want ---\n%s", g, want)
	}
}

// The goldens below pin the exact bytes emitted for a single-stream call. They
// exist so the multi-stream refactor cannot silently change the SDP that
// already-deployed peers negotiate against.

func TestGenerateOffer_Golden(t *testing.T) {
	got := GenerateOffer(SDPConfig{
		LocalIP: "192.0.2.1",
		RTPPort: 10000,
		Codecs:  []codec.CodecType{codec.CodecPCMU, codec.CodecPCMA},
	})
	assertSDP(t, got, `v=0
o=<origin>
s=-
c=IN IP4 192.0.2.1
t=0 0
m=audio 10000 RTP/AVP 0 8 101
a=rtpmap:0 PCMU/8000
a=rtpmap:8 PCMA/8000
a=rtpmap:101 telephone-event/8000
a=fmtp:101 0-16
a=ptime:20
a=sendrecv
a=rtcp-mux
`)
}

func TestGenerateOffer_GoldenOpus(t *testing.T) {
	got := GenerateOffer(SDPConfig{
		LocalIP: "192.0.2.1",
		RTPPort: 10000,
		Codecs:  []codec.CodecType{codec.CodecOpus, codec.CodecPCMU},
	})
	assertSDP(t, got, `v=0
o=<origin>
s=-
c=IN IP4 192.0.2.1
t=0 0
m=audio 10000 RTP/AVP 111 0 100 101
a=rtpmap:111 opus/48000/2
a=fmtp:111 minptime=20; useinbandfec=1; stereo=0; sprop-stereo=0
a=rtpmap:0 PCMU/8000
a=rtpmap:100 telephone-event/48000
a=fmtp:100 0-16
a=rtpmap:101 telephone-event/8000
a=fmtp:101 0-16
a=ptime:20
a=sendrecv
a=rtcp-mux
`)
}

func TestGenerateOffer_GoldenAMRWB(t *testing.T) {
	got := GenerateOffer(SDPConfig{
		LocalIP: "192.0.2.1",
		RTPPort: 10000,
		Codecs:  []codec.CodecType{codec.CodecAMRWB},
	})
	assertSDP(t, got, `v=0
o=<origin>
s=-
c=IN IP4 192.0.2.1
t=0 0
m=audio 10000 RTP/AVP 96 101
a=rtpmap:96 AMR-WB/16000/1
a=rtpmap:101 telephone-event/16000
a=fmtp:101 0-16
a=ptime:20
a=sendrecv
a=rtcp-mux
`)
}

func TestGenerateAnswer_Golden(t *testing.T) {
	got := GenerateAnswer(SDPConfig{
		LocalIP: "192.0.2.1",
		RTPPort: 10002,
	}, codec.CodecPCMA, 8, false)
	assertSDP(t, got, `v=0
o=<origin>
s=-
c=IN IP4 192.0.2.1
t=0 0
m=audio 10002 RTP/AVP 8 101
a=rtpmap:8 PCMA/8000
a=rtpmap:101 telephone-event/8000
a=fmtp:101 0-16
a=ptime:20
a=sendrecv
a=rtcp-mux
`)
}

func TestGenerateAnswer_GoldenRejectedText(t *testing.T) {
	got := GenerateAnswer(SDPConfig{
		LocalIP: "192.0.2.1",
		RTPPort: 10002,
	}, codec.CodecPCMU, 0, true)
	assertSDP(t, got, `v=0
o=<origin>
s=-
c=IN IP4 192.0.2.1
t=0 0
m=audio 10002 RTP/AVP 0 101
a=rtpmap:0 PCMU/8000
a=rtpmap:101 telephone-event/8000
a=fmtp:101 0-16
a=ptime:20
a=sendrecv
a=rtcp-mux
m=text 0 RTP/AVP 0
`)
}

func TestGenerateReInviteSDP_GoldenHold(t *testing.T) {
	got := GenerateReInviteSDP(SDPConfig{
		LocalIP: "192.0.2.1",
		RTPPort: 10002,
	}, codec.CodecPCMU, 0, DirSendOnly)
	assertSDP(t, got, `v=0
o=<origin>
s=-
c=IN IP4 192.0.2.1
t=0 0
m=audio 10002 RTP/AVP 0 101
a=rtpmap:0 PCMU/8000
a=rtpmap:101 telephone-event/8000
a=fmtp:101 0-16
a=ptime:20
a=sendonly
a=rtcp-mux
`)
}

func TestGenerateOffer_GoldenWithText(t *testing.T) {
	got := GenerateOffer(SDPConfig{
		LocalIP:       "192.0.2.1",
		RTPPort:       10000,
		Codecs:        []codec.CodecType{codec.CodecPCMU},
		TextRTPPort:   10004,
		TextT140PT:    99,
		TextREDPT:     98,
		RTTRedundancy: 2,
	})
	assertSDP(t, got, `v=0
o=<origin>
s=-
c=IN IP4 192.0.2.1
t=0 0
m=audio 10000 RTP/AVP 0 101
a=rtpmap:0 PCMU/8000
a=rtpmap:101 telephone-event/8000
a=fmtp:101 0-16
a=ptime:20
a=sendrecv
a=rtcp-mux
m=text 10004 RTP/AVP 98 99
a=rtpmap:98 red/1000
a=rtpmap:99 t140/1000
a=fmtp:98 99/99/99
a=sendrecv
`)
}

// Streams overrides the scalar RTPPort/Codecs path entirely.

func TestGenerateOffer_StreamsMultiAudio(t *testing.T) {
	got := GenerateOffer(SDPConfig{
		LocalIP: "192.0.2.1",
		RTPPort: 10000, // must be ignored once Streams is set
		Codecs:  []codec.CodecType{codec.CodecOpus},
		Streams: []AudioStream{
			{
				Port: 40000, Direction: DirSendRecv,
				Codecs: []codec.CodecType{codec.CodecPCMU},
				MID:    "orig", Label: "1", Content: "main", Lang: "en",
			},
			{
				Port: 40002, Direction: DirSendOnly,
				Codecs: []codec.CodecType{codec.CodecPCMU},
				MID:    "xlat", Label: "2", Content: "alt", Lang: "es",
			},
		},
	})
	assertSDP(t, got, `v=0
o=<origin>
s=-
c=IN IP4 192.0.2.1
t=0 0
m=audio 40000 RTP/AVP 0 101
a=rtpmap:0 PCMU/8000
a=rtpmap:101 telephone-event/8000
a=fmtp:101 0-16
a=ptime:20
a=sendrecv
a=mid:orig
a=label:1
a=content:main
a=lang:en
a=rtcp-mux
m=audio 40002 RTP/AVP 0 101
a=rtpmap:0 PCMU/8000
a=rtpmap:101 telephone-event/8000
a=fmtp:101 0-16
a=ptime:20
a=sendonly
a=mid:xlat
a=label:2
a=content:alt
a=lang:es
a=rtcp-mux
`)
}

func TestBuildAudioMediaDescription_RejectedSection(t *testing.T) {
	got := GenerateAnswer(SDPConfig{
		LocalIP: "192.0.2.1",
		Streams: []AudioStream{
			{Port: 40000, Codecs: []codec.CodecType{codec.CodecPCMU}},
			{Port: 0, MID: "xlat"}, // rejected: no attributes, port 0
		},
	}, codec.CodecPCMU, 0, false)
	assertSDP(t, got, `v=0
o=<origin>
s=-
c=IN IP4 192.0.2.1
t=0 0
m=audio 40000 RTP/AVP 0 101
a=rtpmap:0 PCMU/8000
a=rtpmap:101 telephone-event/8000
a=fmtp:101 0-16
a=ptime:20
a=sendrecv
a=rtcp-mux
m=audio 0 RTP/AVP 0
`)
}

// Round-tripping our own multi-stream offer through ParseSDP is the contract
// the negotiation layer relies on.
func TestGenerateOffer_StreamsRoundTrip(t *testing.T) {
	raw := GenerateOffer(SDPConfig{
		LocalIP: "192.0.2.1",
		Streams: []AudioStream{
			{Port: 40000, Direction: DirSendRecv, Codecs: []codec.CodecType{codec.CodecPCMU}, MID: "orig", Lang: "en"},
			{Port: 0, MID: "gone"},
			{Port: 40004, Direction: DirSendOnly, Codecs: []codec.CodecType{codec.CodecPCMA}, MID: "xlat", Lang: "es"},
		},
	})

	m, err := ParseSDP(raw)
	if err != nil {
		t.Fatalf("ParseSDP: %v", err)
	}
	if len(m.Audio) != 3 || len(m.MLines) != 3 {
		t.Fatalf("want 3 audio sections and 3 m-lines, got %d/%d", len(m.Audio), len(m.MLines))
	}
	if m.PrimaryAudio != 0 || m.RemotePort != 40000 {
		t.Errorf("primary = idx %d port %d, want 0/40000", m.PrimaryAudio, m.RemotePort)
	}
	if m.Audio[1].RemotePort != 0 {
		t.Errorf("tombstone slot must round-trip with port 0, got %d", m.Audio[1].RemotePort)
	}
	third, ok := m.AudioByMID("xlat")
	if !ok {
		t.Fatal("mid xlat did not round-trip")
	}
	if third.RemotePort != 40004 || third.Direction != DirSendOnly || third.Lang != "es" {
		t.Errorf("third section = %+v", third)
	}
}
