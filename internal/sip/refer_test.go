package sip

import "testing"

func TestParseReferTo_Blind(t *testing.T) {
	uri, rp, err := ParseReferTo("<sip:bob@example.com>")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if uri != "sip:bob@example.com" {
		t.Errorf("uri = %q", uri)
	}
	if rp != nil {
		t.Errorf("expected no Replaces, got %+v", rp)
	}
}

func TestParseReferTo_Attended(t *testing.T) {
	value := "<sip:c@example.com?Replaces=abc%40host%3Bto-tag%3Dxx%3Bfrom-tag%3Dyy>"
	uri, rp, err := ParseReferTo(value)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if uri != "sip:c@example.com" {
		t.Errorf("uri = %q", uri)
	}
	if rp == nil {
		t.Fatal("expected Replaces")
	}
	if rp.CallID != "abc@host" {
		t.Errorf("CallID = %q", rp.CallID)
	}
	if rp.ToTag != "xx" {
		t.Errorf("ToTag = %q", rp.ToTag)
	}
	if rp.FromTag != "yy" {
		t.Errorf("FromTag = %q", rp.FromTag)
	}
}

func TestParseReferTo_NoAngles(t *testing.T) {
	uri, rp, err := ParseReferTo("sip:bob@example.com")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if uri != "sip:bob@example.com" || rp != nil {
		t.Errorf("uri=%q rp=%+v", uri, rp)
	}
}

func TestReplacesParams_String(t *testing.T) {
	rp := &ReplacesParams{CallID: "abc@host", ToTag: "xx", FromTag: "yy"}
	got := rp.String()
	want := "abc@host;to-tag=xx;from-tag=yy"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestParseSipfrag(t *testing.T) {
	cases := []struct {
		body     string
		wantCode int
		wantMsg  string
	}{
		{"SIP/2.0 200 OK\r\n", 200, "OK"},
		{"SIP/2.0 100 Trying", 100, "Trying"},
		{"SIP/2.0 486 Busy Here\r\nDate: x", 486, "Busy Here"},
		{"garbage", 0, ""},
		{"", 0, ""},
	}
	for _, c := range cases {
		code, msg := ParseSipfrag([]byte(c.body))
		if code != c.wantCode || msg != c.wantMsg {
			t.Errorf("body %q: got (%d,%q), want (%d,%q)", c.body, code, msg, c.wantCode, c.wantMsg)
		}
	}
}
