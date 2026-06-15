package sip

import (
	"strings"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/codec"
)

func TestAMRNBRtpmapFmtp(t *testing.T) {
	if got := codecRtpmap(codec.CodecAMRNB); got != "AMR/8000/1" {
		t.Errorf("codecRtpmap = %q, want AMR/8000/1", got)
	}
	if got := codecFmtp(codec.CodecAMRNB, false, "", true, ""); got != "octet-align=1" {
		t.Errorf("codecFmtp(octet-aligned) = %q, want octet-align=1", got)
	}
	if got := codecFmtp(codec.CodecAMRNB, false, "", false, ""); got != "" {
		t.Errorf("codecFmtp(bandwidth-efficient) = %q, want empty", got)
	}
	if got := codecFmtp(codec.CodecAMRNB, false, "", true, "0,4,7"); got != "octet-align=1; mode-set=0,4,7" {
		t.Errorf("codecFmtp(octet+mode-set) = %q, want octet-align=1; mode-set=0,4,7", got)
	}
	if got := codecFmtp(codec.CodecAMRNB, false, "", false, "0,4,7"); got != "mode-set=0,4,7" {
		t.Errorf("codecFmtp(be+mode-set) = %q, want mode-set=0,4,7", got)
	}
}

func TestAMRNBModeSetParse(t *testing.T) {
	cases := map[string][]int{
		"octet-align=1; mode-set=0,1,2": {0, 1, 2},
		"mode-set=0,4,7,8":              {0, 4, 7}, // 8 is out of NB range, dropped
		"MODE-SET=2":                    {2},
		"octet-align=1":                 nil,
		"":                              nil,
		"mode-set=8,9,-1":               nil, // all out of range
		"mode-set=2,99,7":               {2, 7},
	}
	for fmtp, want := range cases {
		got := AMRNBModeSet(fmtp)
		if len(got) != len(want) {
			t.Errorf("AMRNBModeSet(%q) = %v, want %v", fmtp, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("AMRNBModeSet(%q) = %v, want %v", fmtp, got, want)
				break
			}
		}
	}
}

func TestClampAMRNBMode(t *testing.T) {
	cases := []struct {
		ceiling int
		set     []int
		want    int
	}{
		{7, []int{0, 1, 2}, 2},    // peer caps below ceiling
		{2, []int{0, 1, 2, 7}, 2}, // ceiling caps below peer's HD
		{1, []int{3, 4}, 3},       // ceiling below set -> lowest member
		{7, nil, 7},               // no restriction -> ceiling
		{7, []int{0, 1, 2, 7}, 7}, // HD wanted and allowed
		{5, []int{0, 2, 7}, 2},    // highest member <= ceiling
	}
	for _, c := range cases {
		if got := ClampAMRNBMode(c.ceiling, c.set); got != c.want {
			t.Errorf("ClampAMRNBMode(%d, %v) = %d, want %d", c.ceiling, c.set, got, c.want)
		}
	}
}

func TestAMRNBOctetAligned(t *testing.T) {
	cases := map[string]bool{
		"octet-align=1":               true,
		"octet-align=1; mode-set=0,4": true,
		"OCTET-ALIGN=1":               true,
		"":                            false,
		"octet-align=0":               false,
		"mode-set=0,4":                false,
	}
	for fmtp, want := range cases {
		if got := AMRNBOctetAligned(fmtp); got != want {
			t.Errorf("AMRNBOctetAligned(%q) = %v, want %v", fmtp, got, want)
		}
	}
}

// TestParseSDPCapturesAMRNBFmtp verifies the SDP parser resolves the RFC 4867
// "AMR" rtpmap name to CodecAMRNB and captures the raw fmtp params.
func TestParseSDPCapturesAMRNBFmtp(t *testing.T) {
	raw := "v=0\r\n" +
		"o=- 1 1 IN IP4 192.0.2.1\r\n" +
		"s=-\r\n" +
		"c=IN IP4 192.0.2.1\r\n" +
		"t=0 0\r\n" +
		"m=audio 5004 RTP/AVP 97\r\n" +
		"a=rtpmap:97 AMR/8000/1\r\n" +
		"a=fmtp:97 octet-align=1; mode-set=0,4,7\r\n"

	m, err := ParseSDP([]byte(raw))
	if err != nil {
		t.Fatalf("ParseSDP: %v", err)
	}
	if len(m.Codecs) != 1 || m.Codecs[0] != codec.CodecAMRNB {
		t.Fatalf("Codecs = %v, want [AMR-NB]", m.Codecs)
	}
	if m.CodecPTs[codec.CodecAMRNB] != 97 {
		t.Errorf("AMR-NB PT = %d, want 97", m.CodecPTs[codec.CodecAMRNB])
	}
	if m.CodecRates[codec.CodecAMRNB] != 8000 {
		t.Errorf("AMR-NB rate = %d, want 8000", m.CodecRates[codec.CodecAMRNB])
	}
	fmtp := m.CodecFmtp[codec.CodecAMRNB]
	if !AMRNBOctetAligned(fmtp) {
		t.Errorf("captured fmtp %q not detected as octet-aligned", fmtp)
	}
	if got := AMRNBModeSet(fmtp); len(got) != 3 || got[0] != 0 || got[1] != 4 || got[2] != 7 {
		t.Errorf("AMRNBModeSet(captured) = %v, want [0 4 7]", got)
	}
}

func TestGenerateOfferIncludesAMRNB(t *testing.T) {
	offer := string(GenerateOffer(SDPConfig{
		LocalIP:           "192.0.2.1",
		RTPPort:           5004,
		Codecs:            []codec.CodecType{codec.CodecPCMU, codec.CodecAMRNB},
		AMRNBOctetAligned: true,
	}))
	if !strings.Contains(offer, "AMR/8000/1") {
		t.Errorf("offer missing AMR-NB rtpmap:\n%s", offer)
	}
	if !strings.Contains(offer, "octet-align=1") {
		t.Errorf("offer missing AMR-NB octet-align fmtp:\n%s", offer)
	}
	// Telephone-event clock rate stays at 8 kHz when AMR-NB (and other 8 kHz
	// codecs) is offered — this is RFC 4733 default.
	if !strings.Contains(offer, "telephone-event/8000") {
		t.Errorf("offer missing 8 kHz telephone-event:\n%s", offer)
	}
}

func TestGenerateAnswerEchoesAMRNBFraming(t *testing.T) {
	aligned := string(GenerateAnswer(SDPConfig{
		LocalIP:           "192.0.2.1",
		RTPPort:           5004,
		Codecs:            []codec.CodecType{codec.CodecAMRNB},
		AMRNBOctetAligned: true,
	}, codec.CodecAMRNB, 97, false))
	if !strings.Contains(aligned, "a=fmtp:97 octet-align=1") {
		t.Errorf("octet-aligned answer missing fmtp:\n%s", aligned)
	}

	be := string(GenerateAnswer(SDPConfig{
		LocalIP:           "192.0.2.1",
		RTPPort:           5004,
		Codecs:            []codec.CodecType{codec.CodecAMRNB},
		AMRNBOctetAligned: false,
	}, codec.CodecAMRNB, 97, false))
	if strings.Contains(be, "octet-align") {
		t.Errorf("bandwidth-efficient answer should not carry octet-align fmtp:\n%s", be)
	}
	if !strings.Contains(be, "AMR/8000/1") {
		t.Errorf("bandwidth-efficient answer missing AMR-NB rtpmap:\n%s", be)
	}

	ans := string(GenerateAnswer(SDPConfig{
		LocalIP:           "192.0.2.1",
		RTPPort:           5004,
		Codecs:            []codec.CodecType{codec.CodecAMRNB},
		AMRNBOctetAligned: true,
		AMRNBModeSet:      "0,4,7",
	}, codec.CodecAMRNB, 97, false))
	if !strings.Contains(ans, "a=fmtp:97 octet-align=1; mode-set=0,4,7") {
		t.Errorf("answer missing mode-set fmtp:\n%s", ans)
	}

	offer := string(GenerateOffer(SDPConfig{
		LocalIP:           "192.0.2.1",
		RTPPort:           5004,
		Codecs:            []codec.CodecType{codec.CodecAMRNB},
		AMRNBOctetAligned: true,
	}))
	if strings.Contains(offer, "mode-set") {
		t.Errorf("offer should not carry mode-set:\n%s", offer)
	}
}

func TestTelephoneEventClockRateAMRNB(t *testing.T) {
	if got := TelephoneEventClockRate(codec.CodecAMRNB); got != 8000 {
		t.Errorf("TelephoneEventClockRate(AMR-NB) = %d, want 8000", got)
	}
}
