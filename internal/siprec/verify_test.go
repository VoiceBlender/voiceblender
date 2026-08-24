package siprec

import (
	"fmt"
	"testing"
)

// metadataXML renders the shape a session-recording client emits for a
// two-party call: participant A on stream s1, participant B on s2, with each
// stream's label supplied by the caller so a test can invert the binding.
func metadataXML(aorA, labelA, aorB, labelB string) []byte {
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<recording xmlns="urn:ietf:params:xml:ns:recording:1">
  <datamode>complete</datamode>
  <session session_id="sess-1"/>
  <participant participant_id="pA" session_id="sess-1"><nameID aor="%s"/></participant>
  <participant participant_id="pB" session_id="sess-1"><nameID aor="%s"/></participant>
  <stream stream_id="s1" session_id="sess-1"><label>%s</label></stream>
  <stream stream_id="s2" session_id="sess-1"><label>%s</label></stream>
  <participantstreamassoc participant_id="pA"><send>s1</send><recv>s2</recv></participantstreamassoc>
  <participantstreamassoc participant_id="pB"><send>s2</send><recv>s1</recv></participantstreamassoc>
</recording>`, aorA, aorB, labelA, labelB))
}

// twoPartyOffer is the shape a session recording client sends: one labelled
// section per party, each naming its sender in a=ssrc cname. The cname hosts
// differ from the AOR domain, as they do when a media relay writes them.
func twoPartyOffer() []MediaSection {
	return []MediaSection{
		{Label: "0", CNAME: "sip:alice@10.0.0.1"},
		{Label: "1", CNAME: "sip:bob@10.0.0.2"},
	}
}

func TestVerify(t *testing.T) {
	const aorA = "sip:alice@example.com"
	const aorB = "sip:bob@example.com"

	cases := []struct {
		name     string
		metadata []byte
		sections []MediaSection
		want     []Issue
	}{
		{
			name:     "labels agree with the offer",
			metadata: metadataXML(aorA, "0", aorB, "1"),
			sections: twoPartyOffer(),
		},
		{
			// Valid and self-consistent, but the offer says otherwise.
			name:     "caller and callee inverted",
			metadata: metadataXML(aorA, "1", aorB, "0"),
			sections: twoPartyOffer(),
			want: []Issue{
				{Kind: IssueParticipantMismatch, Label: "0",
					Detail: "offer says alice sends on it, metadata assigns it to bob (pB)"},
				{Kind: IssueParticipantMismatch, Label: "1",
					Detail: "offer says bob sends on it, metadata assigns it to alice (pA)"},
			},
		},
		{
			// The cname host is written by whatever anchored the media and does
			// not have to match the AOR domain.
			name:     "cname host differs from the AOR domain",
			metadata: metadataXML(aorA, "0", aorB, "1"),
			sections: []MediaSection{
				{Label: "0", CNAME: "sip:alice@10.0.0.1"},
				{Label: "1", CNAME: "sip:bob@10.0.0.2"},
			},
		},
		{
			name:     "offer carries no cname",
			metadata: metadataXML(aorA, "1", aorB, "0"),
			sections: []MediaSection{{Label: "0"}, {Label: "1"}},
		},
		{
			name:     "cname is not a URI",
			metadata: metadataXML(aorA, "1", aorB, "0"),
			sections: []MediaSection{{Label: "0", CNAME: "randomcname"}, {Label: "1", CNAME: "other"}},
		},
		{
			// The binding here is inverted, exactly as in "caller and callee
			// inverted" above — but nothing in this offer says so, and it must
			// not be reported as if something did.
			//
			// An RFC 3550 cname is "<token>@<host>" with the token a
			// synchronisation source identifier, not a person: Chrome writes a
			// random string, plenty of SBCs write the RTP source address.
			// Reading a user part out of one and comparing it to a participant
			// AOR flags every correct session whose media was anchored by a
			// stack that does not write SIP URIs there — a warning on a good
			// recording, which is worse than the missed check, because it is
			// the warning that a controller acts on.
			name:     "an opaque RTCP cname claims nothing",
			metadata: metadataXML(aorA, "1", aorB, "0"),
			sections: []MediaSection{
				{Label: "0", CNAME: "2890844526@10.0.0.1"},
				{Label: "1", CNAME: "j6kZq1nT@192.168.1.7"},
			},
		},
		{
			name:     "both streams claim one label",
			metadata: metadataXML(aorA, "0", aorB, "0"),
			sections: []MediaSection{{Label: "0", CNAME: aorA}},
			want: []Issue{
				{Kind: IssueDuplicateLabel, Label: "0", Detail: "streams s1 and s2 both claim it"},
			},
		},
		{
			// Label 1 is offered and the document says nothing about it, which
			// is not a contradiction — only label 9 is.
			name:     "metadata labels a stream the offer does not carry",
			metadata: metadataXML(aorA, "0", aorB, "9"),
			sections: twoPartyOffer(),
			want: []Issue{
				{Kind: IssueUnknownLabel, Label: "9", Detail: "no m= section in the offer carries this label"},
			},
		},
		{
			// An offer with no labels is no evidence about any label. Reporting
			// every stream as unknown would be noise, not a finding.
			name:     "offer carries no labels at all",
			metadata: metadataXML(aorA, "0", aorB, "1"),
			sections: []MediaSection{{CNAME: aorA}, {CNAME: aorB}},
		},
		{
			name:     "no offer at all",
			metadata: metadataXML(aorA, "0", aorB, "1"),
			sections: nil,
		},
		{
			// tel URIs have no host, so the comparison must not fall back to
			// "no claim made" and silently skip the check.
			name:     "tel URIs, inverted",
			metadata: metadataXML("tel:+43111", "1", "tel:+43222", "0"),
			sections: []MediaSection{
				{Label: "0", CNAME: "tel:+43111"},
				{Label: "1", CNAME: "tel:+43222"},
			},
			want: []Issue{
				{Kind: IssueParticipantMismatch, Label: "0",
					Detail: "offer says +43111 sends on it, metadata assigns it to +43222 (pB)"},
				{Kind: IssueParticipantMismatch, Label: "1",
					Detail: "offer says +43222 sends on it, metadata assigns it to +43111 (pA)"},
			},
		},
		{
			name:     "tel URIs, agreeing",
			metadata: metadataXML("tel:+43111", "0", "tel:+43222", "1"),
			sections: []MediaSection{
				{Label: "0", CNAME: "tel:+43111;phone-context=+43"},
				{Label: "1", CNAME: "tel:+43222"},
			},
		},
		{
			// A section the document says nothing about is not a contradiction;
			// a stream it declares and then nobody sends on is.
			name:     "an offered section the document is silent about",
			metadata: metadataXML(aorA, "0", aorB, "1"),
			sections: append(twoPartyOffer(), MediaSection{Label: "2", CNAME: "sip:carol@example.com"}),
		},
		{
			name: "the document declares a stream nobody sends on",
			metadata: []byte(`<?xml version="1.0" encoding="UTF-8"?>
<recording xmlns="urn:ietf:params:xml:ns:recording:1">
  <participant participant_id="pA"><nameID aor="sip:alice@example.com"/></participant>
  <stream stream_id="s1"><label>0</label></stream>
  <stream stream_id="s2"><label>1</label></stream>
  <participantstreamassoc participant_id="pA"><send>s1</send></participantstreamassoc>
</recording>`),
			sections: twoPartyOffer(),
			want: []Issue{
				{Kind: IssueUnclaimedLabel, Label: "1", Detail: "the metadata declares this stream but no participant sends on it"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, err := Parse(tc.metadata)
			if err != nil {
				t.Fatalf("parse metadata: %v", err)
			}
			got := Verify(rec, tc.sections)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d issues, want %d\n got: %v\nwant: %v", len(got), len(tc.want), got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("issue %d:\n got: %+v\nwant: %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// A participant that has left is no longer the sender, so its section must be
// reported as unclaimed rather than silently keeping the old attribution.
func TestVerifyIgnoresDisassociatedSender(t *testing.T) {
	md := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<recording xmlns="urn:ietf:params:xml:ns:recording:1">
  <participant participant_id="pA"><nameID aor="sip:alice@example.com"/></participant>
  <stream stream_id="s1"><label>0</label></stream>
  <participantstreamassoc participant_id="pA"><send>s1</send><disassociate-time>2026-08-10T10:00:00Z</disassociate-time></participantstreamassoc>
</recording>`)

	rec, err := Parse(md)
	if err != nil {
		t.Fatalf("parse metadata: %v", err)
	}

	got := Verify(rec, []MediaSection{{Label: "0", CNAME: "sip:alice@example.com"}})
	want := []Issue{{Kind: IssueUnclaimedLabel, Label: "0", Detail: "the metadata declares this stream but no participant sends on it"}}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// A party hangs up. The SRC closes its association with a disassociate-time and
// its stream drops out of the session, while the m= section stays in the offer.
// That is what a departure looks like, not a defect in the document — reporting
// it would flag every call somebody leaves early.
func TestVerifyADepartedPartyIsNotADisagreement(t *testing.T) {
	st := NewState()
	complete, err := Parse(metadataXML("sip:alice@example.com", "0", "sip:bob@example.com", "1"))
	if err != nil {
		t.Fatalf("parse complete: %v", err)
	}
	st.Apply(complete)

	sections := []MediaSection{{Label: "0"}, {Label: "1"}}
	if got := Verify(st.Merged(), sections); len(got) != 0 {
		t.Fatalf("a healthy session reported %v", got)
	}

	leave, err := Parse([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<recording xmlns="urn:ietf:params:xml:ns:recording:1">
  <datamode>partial</datamode>
  <participantsessionassoc participant_id="pB"><disassociate-time>2026-08-07T09:18:22Z</disassociate-time></participantsessionassoc>
</recording>`))
	if err != nil {
		t.Fatalf("parse leave: %v", err)
	}
	st.Apply(leave)

	if got := Verify(st.Merged(), sections); len(got) != 0 {
		t.Fatalf("a party leaving reported %v; the section it used is legitimately unclaimed", got)
	}
}

// Two participants cannot both send on one section. Silently keeping one of
// them would make the outcome depend on document order and hide the ambiguity.
func TestVerifyAmbiguousSender(t *testing.T) {
	md := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<recording xmlns="urn:ietf:params:xml:ns:recording:1">
  <participant participant_id="pA"><nameID aor="sip:alice@example.com"/></participant>
  <participant participant_id="pB"><nameID aor="sip:bob@example.com"/></participant>
  <stream stream_id="s1"><label>0</label></stream>
  <participantstreamassoc participant_id="pA"><send>s1</send></participantstreamassoc>
  <participantstreamassoc participant_id="pB"><send>s1</send></participantstreamassoc>
</recording>`)

	rec, err := Parse(md)
	if err != nil {
		t.Fatalf("parse metadata: %v", err)
	}

	// The cname names alice, but the section is not attributable, so the
	// ambiguity is reported and no mismatch is claimed on top of it.
	got := Verify(rec, []MediaSection{{Label: "0", CNAME: "sip:alice@example.com"}})
	want := []Issue{{Kind: IssueAmbiguousSender, Label: "0",
		Detail: "participants pA and pB both send on it"}}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// One participant listed across several associations is not an ambiguity.
func TestVerifyRepeatedSenderIsNotAmbiguous(t *testing.T) {
	md := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<recording xmlns="urn:ietf:params:xml:ns:recording:1">
  <participant participant_id="pA"><nameID aor="sip:alice@example.com"/></participant>
  <stream stream_id="s1"><label>0</label></stream>
  <participantstreamassoc participant_id="pA"><send>s1</send></participantstreamassoc>
  <participantstreamassoc participant_id="pA"><send>s1</send></participantstreamassoc>
</recording>`)

	rec, err := Parse(md)
	if err != nil {
		t.Fatalf("parse metadata: %v", err)
	}

	if got := Verify(rec, []MediaSection{{Label: "0", CNAME: "sip:alice@example.com"}}); len(got) != 0 {
		t.Fatalf("got %v, want no issues", got)
	}
}

func TestVerifyNilRecording(t *testing.T) {
	if got := Verify(nil, twoPartyOffer()); got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}

// Only a cname written as a URI names anybody. Everything else is a
// synchronisation source identifier that happens to be shaped like an address.
func TestCNAMEUser(t *testing.T) {
	cases := map[string]string{
		"sip:alice@example.com":  "alice",
		"sips:alice@example.com": "alice",
		"SIP:Alice@10.0.0.1":     "Alice",
		"tel:+4312345":           "+4312345",

		// The cases aorUser answers and cnameUser must not.
		"alice@example.com":   "",
		"2890844526@10.0.0.1": "",
		"randomcname":         "",
		"":                    "",

		// A scheme that is not a SIP or tel URI says nothing either.
		"https://example.com/alice": "",
	}
	for in, want := range cases {
		if got := cnameUser(in); got != want {
			t.Errorf("cnameUser(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAORUser(t *testing.T) {
	cases := map[string]string{
		"sip:alice@example.com":    "alice",
		"sips:alice@example.com":   "alice",
		"SIP:Alice@example.com":    "Alice",
		"tel:+4312345@example.com": "+4312345",
		"alice@example.com":        "alice",
		"randomcname":              "",
		"":                         "",
		"   ":                      "",
		"sip:bob@1.2.3.4":          "bob",

		// A tel URI carries no host (RFC 3966).
		"tel:+4312345":                   "+4312345",
		"TEL:+4312345;phone-context=+43": "+4312345",
		// A sip URI without a user part names a host, which identifies nobody.
		"sip:example.com": "",
	}
	for in, want := range cases {
		if got := aorUser(in); got != want {
			t.Errorf("aorUser(%q) = %q, want %q", in, got, want)
		}
	}
}
