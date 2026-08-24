package sip

import "testing"

// A SIPREC offer names the sender of each section in a=ssrc cname, which is
// the only place the SDP itself says who is on which label.
func TestParseSDPSSRCCNAME(t *testing.T) {
	offer := []byte("v=0\r\n" +
		"o=- 1 1 IN IP4 10.0.0.1\r\n" +
		"s=recording\r\n" +
		"c=IN IP4 10.0.0.1\r\n" +
		"t=0 0\r\n" +
		"m=audio 20132 RTP/AVP 0\r\n" +
		"a=rtpmap:0 PCMU/8000\r\n" +
		"a=label:0\r\n" +
		"a=sendonly\r\n" +
		"a=ssrc:3881494032 cname:sip:alice@example.com\r\n" +
		"m=audio 20082 RTP/AVP 0\r\n" +
		"a=rtpmap:0 PCMU/8000\r\n" +
		"a=label:1\r\n" +
		"a=sendonly\r\n" +
		"a=ssrc:274728081 cname:sip:bob@10.0.0.2\r\n" +
		"a=ssrc:274728081 label:ignored\r\n" +
		"m=audio 20090 RTP/AVP 0\r\n" +
		"a=rtpmap:0 PCMU/8000\r\n" +
		"a=label:2\r\n")

	sdp, err := ParseSDP(offer)
	if err != nil {
		t.Fatalf("ParseSDP: %v", err)
	}
	if len(sdp.Audio) != 3 {
		t.Fatalf("got %d audio sections, want 3", len(sdp.Audio))
	}

	want := []struct{ label, cname string }{
		{"0", "sip:alice@example.com"},
		{"1", "sip:bob@10.0.0.2"},
		{"2", ""},
	}
	for i, w := range want {
		if got := sdp.Audio[i].Label; got != w.label {
			t.Errorf("section %d label = %q, want %q", i, got, w.label)
		}
		if got := sdp.Audio[i].CNAME; got != w.cname {
			t.Errorf("section %d cname = %q, want %q", i, got, w.cname)
		}
	}
}

func TestSSRCCNAME(t *testing.T) {
	cases := map[string]string{
		"3881494032 cname:sip:alice@example.com": "sip:alice@example.com",
		"123 msid:a b":                           "",
		"123":                                    "",
		"":                                       "",
		"123 cname:":                             "",
		"123\tcname:sip:alice@example.com":       "sip:alice@example.com",
		"123  cname:sip:bob@example.com":         "sip:bob@example.com",
	}
	for in, want := range cases {
		if got := ssrcCNAME(in); got != want {
			t.Errorf("ssrcCNAME(%q) = %q, want %q", in, got, want)
		}
	}
}
