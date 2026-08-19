package siprec

import "testing"

// A partial update carries only what changed, so verification has to run
// against the accumulated session rather than the document that arrived.
func TestMergedAccumulatesPartialUpdates(t *testing.T) {
	st := NewState()

	complete, err := Parse(metadataXML("sip:alice@example.com", "0", "sip:bob@example.com", "1"))
	if err != nil {
		t.Fatalf("parse complete: %v", err)
	}
	st.Apply(complete)

	partial, err := Parse([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<recording xmlns="urn:ietf:params:xml:ns:recording:1">
  <datamode>partial</datamode>
  <participant participant_id="pC"><nameID aor="sip:carol@example.com"/></participant>
  <stream stream_id="s3"><label>2</label></stream>
  <participantstreamassoc participant_id="pC"><send>s3</send></participantstreamassoc>
</recording>`))
	if err != nil {
		t.Fatalf("parse partial: %v", err)
	}
	st.Apply(partial)

	offer := []MediaSection{
		{Label: "0", CNAME: "sip:alice@example.com"},
		{Label: "1", CNAME: "sip:bob@example.com"},
		{Label: "2", CNAME: "sip:carol@example.com"},
	}
	if got := Verify(st.Merged(), offer); len(got) != 0 {
		t.Fatalf("merged session reported %v, want none", got)
	}

	// What merging buys: a partial update names a participant an earlier
	// document declared, so the delta alone carries no AOR to compare the
	// offer's cname against and nothing can be proved from it. Reassigning
	// label 0 to Bob contradicts an offer that says Alice sends on it, and only
	// the accumulated session can show that.
	reassign, err := Parse([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<recording xmlns="urn:ietf:params:xml:ns:recording:1">
  <datamode>partial</datamode>
  <participantstreamassoc participant_id="pB"><send>s1</send></participantstreamassoc>
</recording>`))
	if err != nil {
		t.Fatalf("parse reassign: %v", err)
	}
	if got := Verify(reassign, offer); len(got) != 0 {
		t.Fatalf("the delta alone reported %v; it cannot know who pB is", got)
	}
	st.Apply(reassign)
	got := Verify(st.Merged(), offer)
	if len(got) != 1 || got[0].Kind != IssueParticipantMismatch || got[0].Label != "0" {
		t.Fatalf("merged session reported %v, want a participant_mismatch on label 0", got)
	}

	merged := st.Merged()
	if len(merged.Participants) != 3 {
		t.Errorf("participants = %d, want 3", len(merged.Participants))
	}
	if len(merged.Streams) != 3 {
		t.Errorf("streams = %d, want 3", len(merged.Streams))
	}
	if merged.DataMode != DataModeComplete {
		t.Errorf("data mode = %q, want complete", merged.DataMode)
	}
}

// A stream two participants claim must survive into the merged view, or the
// ambiguity is lost the moment it is applied.
func TestMergedKeepsConflictingSenders(t *testing.T) {
	st := NewState()
	rec, err := Parse([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<recording xmlns="urn:ietf:params:xml:ns:recording:1">
  <participant participant_id="pA"><nameID aor="sip:alice@example.com"/></participant>
  <participant participant_id="pB"><nameID aor="sip:bob@example.com"/></participant>
  <stream stream_id="s1"><label>0</label></stream>
  <participantstreamassoc participant_id="pA"><send>s1</send></participantstreamassoc>
  <participantstreamassoc participant_id="pB"><send>s1</send></participantstreamassoc>
</recording>`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	st.Apply(rec)

	got := Verify(st.Merged(), []MediaSection{{Label: "0", CNAME: "sip:alice@example.com"}})
	want := Issue{Kind: IssueAmbiguousSender, Label: "0",
		Detail: "participants pA and pB both send on it"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("got %v, want [%v]", got, want)
	}
}

// A later document reassigning a stream is not a conflict: only two claims
// inside one document are.
func TestMergedReassignmentIsNotAConflict(t *testing.T) {
	st := NewState()
	first, _ := Parse([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<recording xmlns="urn:ietf:params:xml:ns:recording:1">
  <participant participant_id="pA"><nameID aor="sip:alice@example.com"/></participant>
  <stream stream_id="s1"><label>0</label></stream>
  <participantstreamassoc participant_id="pA"><send>s1</send></participantstreamassoc>
</recording>`))
	st.Apply(first)

	second, _ := Parse([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<recording xmlns="urn:ietf:params:xml:ns:recording:1">
  <datamode>partial</datamode>
  <participant participant_id="pB"><nameID aor="sip:bob@example.com"/></participant>
  <participantstreamassoc participant_id="pB"><send>s1</send></participantstreamassoc>
</recording>`))
	st.Apply(second)

	if got := Verify(st.Merged(), []MediaSection{{Label: "0", CNAME: "sip:bob@example.com"}}); len(got) != 0 {
		t.Fatalf("got %v, want none: a reassignment is not an ambiguity", got)
	}
}

// An ambiguity is only resolved by a document that speaks for the stream again.
// An unrelated partial update must not make it disappear.
func TestMergedConflictSurvivesUnrelatedUpdate(t *testing.T) {
	conflicting := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<recording xmlns="urn:ietf:params:xml:ns:recording:1">
  <participant participant_id="pA"><nameID aor="sip:alice@example.com"/></participant>
  <participant participant_id="pB"><nameID aor="sip:bob@example.com"/></participant>
  <stream stream_id="s1"><label>0</label></stream>
  <participantstreamassoc participant_id="pA"><send>s1</send></participantstreamassoc>
  <participantstreamassoc participant_id="pB"><send>s1</send></participantstreamassoc>
</recording>`)
	offer := []MediaSection{{Label: "0", CNAME: "sip:alice@example.com"}}
	want := Issue{Kind: IssueAmbiguousSender, Label: "0",
		Detail: "participants pA and pB both send on it"}

	st := NewState()
	rec, err := Parse(conflicting)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	st.Apply(rec)
	if got := Verify(st.Merged(), offer); len(got) != 1 || got[0] != want {
		t.Fatalf("before the update: got %v, want [%v]", got, want)
	}

	// A rename, saying nothing about s1. Nothing has resolved the ambiguity.
	rename, err := Parse([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<recording xmlns="urn:ietf:params:xml:ns:recording:1">
  <datamode>partial</datamode>
  <participant participant_id="pA"><nameID aor="sip:alice@example.com"><name>Alice</name></nameID></participant>
</recording>`))
	if err != nil {
		t.Fatalf("parse rename: %v", err)
	}
	st.Apply(rename)

	if got := Verify(st.Merged(), offer); len(got) != 1 || got[0] != want {
		t.Fatalf("after an unrelated update: got %v, want [%v]", got, want)
	}
}

// A document that speaks for the stream again, with one sender, resolves it.
func TestMergedConflictResolvedByRestatement(t *testing.T) {
	st := NewState()
	rec, err := Parse([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<recording xmlns="urn:ietf:params:xml:ns:recording:1">
  <participant participant_id="pA"><nameID aor="sip:alice@example.com"/></participant>
  <participant participant_id="pB"><nameID aor="sip:bob@example.com"/></participant>
  <stream stream_id="s1"><label>0</label></stream>
  <participantstreamassoc participant_id="pA"><send>s1</send></participantstreamassoc>
  <participantstreamassoc participant_id="pB"><send>s1</send></participantstreamassoc>
</recording>`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	st.Apply(rec)

	fixed, err := Parse([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<recording xmlns="urn:ietf:params:xml:ns:recording:1">
  <datamode>partial</datamode>
  <participantstreamassoc participant_id="pA"><send>s1</send></participantstreamassoc>
</recording>`))
	if err != nil {
		t.Fatalf("parse fixed: %v", err)
	}
	st.Apply(fixed)

	if got := Verify(st.Merged(), []MediaSection{{Label: "0", CNAME: "sip:alice@example.com"}}); len(got) != 0 {
		t.Fatalf("got %v, want none once the stream is restated with one sender", got)
	}
}

// A complete document replaces the session, conflicts included.
func TestMergedCompleteDocumentClearsConflict(t *testing.T) {
	st := NewState()
	rec, err := Parse([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<recording xmlns="urn:ietf:params:xml:ns:recording:1">
  <participant participant_id="pA"><nameID aor="sip:alice@example.com"/></participant>
  <participant participant_id="pB"><nameID aor="sip:bob@example.com"/></participant>
  <stream stream_id="s1"><label>0</label></stream>
  <participantstreamassoc participant_id="pA"><send>s1</send></participantstreamassoc>
  <participantstreamassoc participant_id="pB"><send>s1</send></participantstreamassoc>
</recording>`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	st.Apply(rec)

	clean, err := Parse(metadataXML("sip:alice@example.com", "0", "sip:bob@example.com", "1"))
	if err != nil {
		t.Fatalf("parse clean: %v", err)
	}
	st.Apply(clean)

	if got := Verify(st.Merged(), twoPartyOffer()); len(got) != 0 {
		t.Fatalf("got %v, want none after a complete document", got)
	}
}
