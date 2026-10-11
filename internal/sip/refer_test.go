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

func TestBuildReferTo(t *testing.T) {
	rp := &ReplacesParams{CallID: "abc@host", ToTag: "xx", FromTag: "yy"}
	cases := []struct {
		name     string
		referTo  string
		replaces *ReplacesParams
		headers  map[string]string
		want     string
	}{
		{"blind, no headers", "sip:bob@example.com", nil, nil, "sip:bob@example.com"},
		{"attended, no headers", "sip:c@example.com", rp, nil,
			"sip:c@example.com?Replaces=abc%40host%3Bto-tag%3Dxx%3Bfrom-tag%3Dyy"},
		{"headers sorted by name", "sip:bob@example.com", nil,
			map[string]string{"x-inin-cnv": "b2de3fe4-f097", "X-Conversation-ID": "55248d67"},
			"sip:bob@example.com?X-Conversation-ID=55248d67&x-inin-cnv=b2de3fe4-f097"},
		{"reserved characters escaped", "sip:bob@example.com", nil,
			map[string]string{"X-Note": "a b&c=d?e>f;g%h+i"},
			"sip:bob@example.com?X-Note=a%20b%26c%3Dd%3Fe%3Ef%3Bg%25h%2Bi"},
		{"replaces and headers", "sip:c@example.com", rp, map[string]string{"X-A": "1"},
			"sip:c@example.com?Replaces=abc%40host%3Bto-tag%3Dxx%3Bfrom-tag%3Dyy&X-A=1"},
		{"keeps headers already on the URI", "sip:bob@example.com;transport=tcp?X-Old=1", nil,
			map[string]string{"X-New": "2"}, "sip:bob@example.com;transport=tcp?X-Old=1&X-New=2"},
	}
	for _, c := range cases {
		if got := BuildReferTo(c.referTo, c.replaces, c.headers); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestBuildReferTo_ReplacesStillParses(t *testing.T) {
	rp := &ReplacesParams{CallID: "abc@host", ToTag: "xx", FromTag: "yy"}
	uri, got, err := ParseReferTo("<" + BuildReferTo("sip:c@example.com", rp, map[string]string{"X-A": "1"}) + ">")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if uri != "sip:c@example.com" || got == nil || *got != *rp {
		t.Errorf("uri=%q replaces=%+v", uri, got)
	}
}

func TestIsHeaderToken(t *testing.T) {
	for _, ok := range []string{"x-inin-cnv", "X-Conversation-ID", "User-to-User", "P.Asserted_1"} {
		if !IsHeaderToken(ok) {
			t.Errorf("%q should be a valid header name", ok)
		}
	}
	for _, bad := range []string{"", "X Foo", "X:Foo", "X-Foo\r\nVia", "X=1", "X&Y"} {
		if IsHeaderToken(bad) {
			t.Errorf("%q should be rejected", bad)
		}
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
