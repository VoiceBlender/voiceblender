package sip

import (
	"strings"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/codec"
)

func TestAMRWBRtpmapFmtp(t *testing.T) {
	if got := codecRtpmap(codec.CodecAMRWB); got != "AMR-WB/16000/1" {
		t.Errorf("codecRtpmap = %q, want AMR-WB/16000/1", got)
	}
	if got := codecFmtp(codec.CodecAMRWB, true, "", false, ""); got != "octet-align=1" {
		t.Errorf("codecFmtp(octet-aligned) = %q, want octet-align=1", got)
	}
	if got := codecFmtp(codec.CodecAMRWB, false, "", false, ""); got != "" {
		t.Errorf("codecFmtp(bandwidth-efficient) = %q, want empty", got)
	}
	if got := codecFmtp(codec.CodecAMRWB, true, "0,1,2", false, ""); got != "octet-align=1; mode-set=0,1,2" {
		t.Errorf("codecFmtp(octet+mode-set) = %q, want octet-align=1; mode-set=0,1,2", got)
	}
	if got := codecFmtp(codec.CodecAMRWB, false, "0,1,2", false, ""); got != "mode-set=0,1,2" {
		t.Errorf("codecFmtp(be+mode-set) = %q, want mode-set=0,1,2", got)
	}
}

func TestAMRWBModeSetParse(t *testing.T) {
	cases := map[string][]int{
		"octet-align=1; mode-set=0,1,2": {0, 1, 2},
		"mode-set=0,1,2,8":              {0, 1, 2, 8},
		"MODE-SET=2":                    {2},
		"octet-align=1":                 nil, // absent
		"":                              nil,
		"mode-set=9,10,-1":              nil, // all out of range -> dropped -> empty slice
		"mode-set=2,99,3":               {2, 3},
	}
	for fmtp, want := range cases {
		got := AMRWBModeSet(fmtp)
		if len(got) != len(want) {
			t.Errorf("AMRWBModeSet(%q) = %v, want %v", fmtp, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("AMRWBModeSet(%q) = %v, want %v", fmtp, got, want)
				break
			}
		}
	}
}

func TestClampAMRWBMode(t *testing.T) {
	cases := []struct {
		ceiling int
		set     []int
		want    int
	}{
		{8, []int{0, 1, 2}, 2},    // peer caps at 12.65
		{2, []int{0, 1, 2, 8}, 2}, // our ceiling caps below peer's HD
		{2, []int{3, 4}, 3},       // ceiling below all -> lowest in-set
		{8, nil, 8},               // no restriction -> ceiling
		{8, []int{0, 1, 2, 8}, 8}, // HD allowed and wanted
		{5, []int{0, 2, 8}, 2},    // highest member <= ceiling
	}
	for _, c := range cases {
		if got := ClampAMRWBMode(c.ceiling, c.set); got != c.want {
			t.Errorf("ClampAMRWBMode(%d, %v) = %d, want %d", c.ceiling, c.set, got, c.want)
		}
	}
}

func TestAMRWBOctetAligned(t *testing.T) {
	cases := map[string]bool{
		"octet-align=1":               true,
		"octet-align=1; mode-set=0,2": true,
		"OCTET-ALIGN=1":               true,
		"":                            false,
		"octet-align=0":               false,
		"mode-set=0,2":                false,
	}
	for fmtp, want := range cases {
		if got := AMRWBOctetAligned(fmtp); got != want {
			t.Errorf("AMRWBOctetAligned(%q) = %v, want %v", fmtp, got, want)
		}
	}
}

func TestParseSDPCapturesAMRWBFmtp(t *testing.T) {
	raw := "v=0\r\n" +
		"o=- 1 1 IN IP4 192.0.2.1\r\n" +
		"s=-\r\n" +
		"c=IN IP4 192.0.2.1\r\n" +
		"t=0 0\r\n" +
		"m=audio 5004 RTP/AVP 96\r\n" +
		"a=rtpmap:96 AMR-WB/16000/1\r\n" +
		"a=fmtp:96 octet-align=1; mode-set=0,1,2\r\n"

	m, err := ParseSDP([]byte(raw))
	if err != nil {
		t.Fatalf("ParseSDP: %v", err)
	}
	if len(m.Codecs) != 1 || m.Codecs[0] != codec.CodecAMRWB {
		t.Fatalf("Codecs = %v, want [AMR-WB]", m.Codecs)
	}
	if m.CodecPTs[codec.CodecAMRWB] != 96 {
		t.Errorf("AMR-WB PT = %d, want 96", m.CodecPTs[codec.CodecAMRWB])
	}
	if m.CodecRates[codec.CodecAMRWB] != 16000 {
		t.Errorf("AMR-WB rate = %d, want 16000", m.CodecRates[codec.CodecAMRWB])
	}
	fmtp := m.CodecFmtp[codec.CodecAMRWB]
	if !AMRWBOctetAligned(fmtp) {
		t.Errorf("captured fmtp %q not detected as octet-aligned", fmtp)
	}
}

func TestGenerateOfferIncludesAMRWB(t *testing.T) {
	offer := string(GenerateOffer(SDPConfig{
		LocalIP:           "192.0.2.1",
		RTPPort:           5004,
		Codecs:            []codec.CodecType{codec.CodecOpus, codec.CodecAMRWB},
		AMRWBOctetAligned: true,
	}))
	if !strings.Contains(offer, "AMR-WB/16000/1") {
		t.Errorf("offer missing AMR-WB rtpmap:\n%s", offer)
	}
	if !strings.Contains(offer, "octet-align=1") {
		t.Errorf("offer missing AMR-WB octet-align fmtp:\n%s", offer)
	}
}

func TestGenerateAnswerEchoesAMRWBFraming(t *testing.T) {
	// Octet-aligned answer carries the octet-align=1 fmtp.
	aligned := string(GenerateAnswer(SDPConfig{
		LocalIP:           "192.0.2.1",
		RTPPort:           5004,
		Codecs:            []codec.CodecType{codec.CodecAMRWB},
		AMRWBOctetAligned: true,
	}, codec.CodecAMRWB, 97, false))
	if !strings.Contains(aligned, "a=fmtp:97 octet-align=1") {
		t.Errorf("octet-aligned answer missing fmtp:\n%s", aligned)
	}

	// Bandwidth-efficient answer omits the octet-align fmtp entirely.
	be := string(GenerateAnswer(SDPConfig{
		LocalIP:           "192.0.2.1",
		RTPPort:           5004,
		Codecs:            []codec.CodecType{codec.CodecAMRWB},
		AMRWBOctetAligned: false,
	}, codec.CodecAMRWB, 97, false))
	if strings.Contains(be, "octet-align") {
		t.Errorf("bandwidth-efficient answer should not carry octet-align fmtp:\n%s", be)
	}
	if !strings.Contains(be, "AMR-WB/16000/1") {
		t.Errorf("bandwidth-efficient answer missing AMR-WB rtpmap:\n%s", be)
	}

	// Answer echoes the negotiated mode-set; offer omits it.
	ans := string(GenerateAnswer(SDPConfig{
		LocalIP:           "192.0.2.1",
		RTPPort:           5004,
		Codecs:            []codec.CodecType{codec.CodecAMRWB},
		AMRWBOctetAligned: true,
		AMRWBModeSet:      "0,1,2",
	}, codec.CodecAMRWB, 97, false))
	if !strings.Contains(ans, "a=fmtp:97 octet-align=1; mode-set=0,1,2") {
		t.Errorf("answer missing mode-set fmtp:\n%s", ans)
	}
	offer := string(GenerateOffer(SDPConfig{
		LocalIP:           "192.0.2.1",
		RTPPort:           5004,
		Codecs:            []codec.CodecType{codec.CodecAMRWB},
		AMRWBOctetAligned: true,
	}))
	if strings.Contains(offer, "mode-set") {
		t.Errorf("offer should not carry mode-set:\n%s", offer)
	}
}
