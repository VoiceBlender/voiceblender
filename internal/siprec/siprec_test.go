package siprec

import (
	"os"
	"path/filepath"
	"testing"
)

func loadTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read testdata %s: %v", name, err)
	}
	return b
}

func parseFile(t *testing.T, name string) *Recording {
	t.Helper()
	r, err := Parse(loadTestdata(t, name))
	if err != nil {
		t.Fatalf("Parse(%s) = %v, want nil", name, err)
	}
	return r
}

const (
	alice  = "srfBElmCRp2QB23b7Mpluw=="
	bob    = "zSfPoSGiSVKGsxDT2SBjRQ=="
	carol  = "Cj5tXQwGSJm9K1lPuo0Rug=="
	aliceS = "UAAMm5GRQKSCMVvLyl4rFw=="
	bobS   = "i1Pz3to5hGk8fuXl+PbwCw=="
)

func TestParse_CompleteDocument(t *testing.T) {
	r := parseFile(t, "complete.xml")

	if r.DataMode != DataModeComplete {
		t.Fatalf("DataMode = %q, want %q", r.DataMode, DataModeComplete)
	}
	if r.IsPartial() {
		t.Fatal("IsPartial() = true, want false")
	}
	if len(r.Participants) != 2 {
		t.Fatalf("len(Participants) = %d, want 2", len(r.Participants))
	}
	if len(r.Streams) != 2 {
		t.Fatalf("len(Streams) = %d, want 2", len(r.Streams))
	}
	if len(r.ParticipantStreams) != 2 {
		t.Fatalf("len(ParticipantStreams) = %d, want 2", len(r.ParticipantStreams))
	}

	p := r.Participants[0]
	if p.ParticipantID != alice {
		t.Fatalf("Participants[0].ParticipantID = %q, want %q", p.ParticipantID, alice)
	}
	if got := p.AOR(); got != "sip:alice@example.com" {
		t.Fatalf("AOR() = %q, want %q", got, "sip:alice@example.com")
	}
	if got := p.DisplayName(); got != "Alice Smith" {
		t.Fatalf("DisplayName() = %q, want %q", got, "Alice Smith")
	}
	if got := p.NameIDs[0].Name.Lang; got != "en" {
		t.Fatalf("xml:lang = %q, want %q", got, "en")
	}

	if r.Streams[0].Label != "1" || r.Streams[0].StreamID != aliceS {
		t.Fatalf("Streams[0] = %+v, want label 1 with Alice's stream ID", r.Streams[0])
	}
	if got := r.ParticipantStreams[0].Send; len(got) != 1 || got[0] != aliceS {
		t.Fatalf("ParticipantStreams[0].Send = %v, want [%s]", got, aliceS)
	}
	if got := r.ParticipantStreams[0].Recv; len(got) != 1 || got[0] != bobS {
		t.Fatalf("ParticipantStreams[0].Recv = %v, want [%s]", got, bobS)
	}
	if got := r.SessionRecordingAssocs[0].SessionID; got == "" {
		t.Fatal("SessionRecordingAssocs[0].SessionID is empty")
	}
}

func TestParse_NamespaceOptional(t *testing.T) {
	r := parseFile(t, "no_namespace.xml")
	if len(r.Participants) != 1 {
		t.Fatalf("len(Participants) = %d, want 1 without a declared namespace", len(r.Participants))
	}
	if got := r.Participants[0].AOR(); got != "sip:dave@example.com" {
		t.Fatalf("AOR() = %q, want %q", got, "sip:dave@example.com")
	}
}

func TestParse_Errors(t *testing.T) {
	if _, err := Parse(nil); err == nil {
		t.Fatal("Parse(nil) = nil error, want an error")
	}
	if _, err := Parse([]byte("<notrecording/>")); err == nil {
		t.Fatal("Parse of a foreign root = nil error, want an error")
	}
	if _, err := Parse([]byte("<recording>")); err == nil {
		t.Fatal("Parse of a truncated document = nil error, want an error")
	}
}

func TestParse_StripsBOM(t *testing.T) {
	raw := append([]byte{0xEF, 0xBB, 0xBF}, loadTestdata(t, "complete.xml")...)
	if _, err := Parse(raw); err != nil {
		t.Fatalf("Parse with a UTF-8 BOM = %v, want nil", err)
	}
}

func TestMarshal_RoundTrip(t *testing.T) {
	orig := parseFile(t, "complete.xml")

	raw, err := orig.Marshal()
	if err != nil {
		t.Fatalf("Marshal = %v, want nil", err)
	}
	again, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse of marshalled document = %v, want nil", err)
	}

	if again.DataMode != orig.DataMode {
		t.Fatalf("DataMode = %q, want %q", again.DataMode, orig.DataMode)
	}
	if len(again.Participants) != len(orig.Participants) {
		t.Fatalf("len(Participants) = %d, want %d", len(again.Participants), len(orig.Participants))
	}
	if again.Participants[0].AOR() != orig.Participants[0].AOR() {
		t.Fatalf("AOR = %q, want %q", again.Participants[0].AOR(), orig.Participants[0].AOR())
	}
	if again.Participants[0].DisplayName() != orig.Participants[0].DisplayName() {
		t.Fatalf("DisplayName = %q, want %q",
			again.Participants[0].DisplayName(), orig.Participants[0].DisplayName())
	}
	if len(again.Streams) != len(orig.Streams) || again.Streams[0].Label != orig.Streams[0].Label {
		t.Fatalf("streams did not survive the round trip: %+v", again.Streams)
	}
	if len(again.ParticipantStreams[0].Send) != 1 ||
		again.ParticipantStreams[0].Send[0] != orig.ParticipantStreams[0].Send[0] {
		t.Fatalf("send association did not survive: %+v", again.ParticipantStreams[0])
	}

	// Marshalling must declare the registered namespace even when the source
	// document omitted it.
	bare := parseFile(t, "no_namespace.xml")
	out, err := bare.Marshal()
	if err != nil {
		t.Fatalf("Marshal = %v, want nil", err)
	}
	if !contains(out, Namespace) {
		t.Fatalf("marshalled document does not declare %s:\n%s", Namespace, out)
	}
}

func contains(hay []byte, needle string) bool {
	return len(needle) == 0 || len(hay) >= len(needle) &&
		func() bool {
			for i := 0; i+len(needle) <= len(hay); i++ {
				if string(hay[i:i+len(needle)]) == needle {
					return true
				}
			}
			return false
		}()
}

func TestState_ApplyComplete(t *testing.T) {
	s := NewState()
	delta := s.Apply(parseFile(t, "complete.xml"))

	if len(delta.ParticipantsAdded) != 2 {
		t.Fatalf("ParticipantsAdded = %v, want 2 entries", delta.ParticipantsAdded)
	}
	if len(delta.StreamsAdded) != 2 {
		t.Fatalf("StreamsAdded = %v, want 2 entries", delta.StreamsAdded)
	}
	if !delta.Changed() {
		t.Fatal("Changed() = false, want true")
	}
	if got := s.SessionID(); got != "hVpd7YQgRW2nD22h7q60JQ==" {
		t.Fatalf("SessionID() = %q, want the sessionrecordingassoc session ID", got)
	}

	p, ok := s.ParticipantForLabel("1")
	if !ok {
		t.Fatal(`ParticipantForLabel("1") = (_, false), want true`)
	}
	if p.ID != alice || p.AOR != "sip:alice@example.com" || p.Name != "Alice Smith" {
		t.Fatalf(`ParticipantForLabel("1") = %+v, want Alice`, p)
	}

	p2, ok := s.ParticipantForLabel("2")
	if !ok || p2.ID != bob {
		t.Fatalf(`ParticipantForLabel("2") = (%+v, %v), want Bob`, p2, ok)
	}

	if _, ok := s.ParticipantForLabel("99"); ok {
		t.Fatal(`ParticipantForLabel("99") = (_, true), want false`)
	}
}

func TestState_ApplyCompleteReplaces(t *testing.T) {
	s := NewState()
	s.Apply(parseFile(t, "complete.xml"))
	s.Apply(parseFile(t, "partial_join.xml"))

	// A second complete document is the whole truth: Carol must disappear.
	delta := s.Apply(parseFile(t, "complete.xml"))

	if got := delta.ParticipantsRemoved; len(got) != 1 || got[0] != carol {
		t.Fatalf("ParticipantsRemoved = %v, want [%s]", got, carol)
	}
	if _, ok := s.ParticipantForLabel("3"); ok {
		t.Fatal(`ParticipantForLabel("3") = (_, true) after a complete replace, want false`)
	}
	snap := s.Snapshot()
	if len(snap.Participants) != 2 {
		t.Fatalf("len(Participants) = %d after replace, want 2", len(snap.Participants))
	}
}

func TestState_ApplyPartialJoin(t *testing.T) {
	s := NewState()
	s.Apply(parseFile(t, "complete.xml"))

	delta := s.Apply(parseFile(t, "partial_join.xml"))

	if got := delta.ParticipantsAdded; len(got) != 1 || got[0] != carol {
		t.Fatalf("ParticipantsAdded = %v, want [%s]", got, carol)
	}
	if len(delta.ParticipantsRemoved) != 0 {
		t.Fatalf("ParticipantsRemoved = %v, want empty: a partial update never deletes on absence",
			delta.ParticipantsRemoved)
	}

	// The earlier participants must survive the merge.
	if _, ok := s.ParticipantForLabel("1"); !ok {
		t.Fatal(`ParticipantForLabel("1") = (_, false) after a partial update, want true`)
	}
	p, ok := s.ParticipantForLabel("3")
	if !ok || p.ID != carol || p.Name != "Carol Danvers" {
		t.Fatalf(`ParticipantForLabel("3") = (%+v, %v), want Carol`, p, ok)
	}
}

func TestState_ApplyPartialLeave(t *testing.T) {
	s := NewState()
	s.Apply(parseFile(t, "complete.xml"))

	delta := s.Apply(parseFile(t, "partial_leave.xml"))

	if got := delta.ParticipantsRemoved; len(got) != 1 || got[0] != bob {
		t.Fatalf("ParticipantsRemoved = %v, want [%s]", got, bob)
	}
	if got := delta.StreamsRemoved; len(got) != 1 || got[0] != bobS {
		t.Fatalf("StreamsRemoved = %v, want [%s]: a leaver's stream stops carrying audio", got, bobS)
	}
	if _, ok := s.ParticipantForLabel("2"); ok {
		t.Fatal(`ParticipantForLabel("2") = (_, true) after the leave, want false`)
	}
	if _, ok := s.ParticipantForLabel("1"); !ok {
		t.Fatal(`ParticipantForLabel("1") = (_, false), want the remaining participant intact`)
	}
}

func TestState_ApplyIsIdempotent(t *testing.T) {
	s := NewState()
	s.Apply(parseFile(t, "complete.xml"))

	delta := s.Apply(parseFile(t, "complete.xml"))
	if delta.Changed() {
		t.Fatalf("re-applying the same document reported changes: %+v", delta)
	}
}

func TestState_Snapshot(t *testing.T) {
	s := NewState()
	s.Apply(parseFile(t, "complete.xml"))
	s.SetRaw(loadTestdata(t, "complete.xml"))

	snap := s.Snapshot()
	if len(snap.Participants) != 2 || len(snap.Streams) != 2 {
		t.Fatalf("snapshot = %d participants / %d streams, want 2 / 2",
			len(snap.Participants), len(snap.Streams))
	}
	// Ordered deterministically by label.
	if snap.Streams[0].Label != "1" || snap.Streams[1].Label != "2" {
		t.Fatalf("streams are not label-ordered: %+v", snap.Streams)
	}
	if snap.Streams[0].Participant.ID != alice {
		t.Fatalf("Streams[0].Participant.ID = %q, want Alice", snap.Streams[0].Participant.ID)
	}
	if snap.Metadata == "" {
		t.Fatal("Snapshot().Metadata is empty, want the raw document")
	}
	if snap.SessionID == "" {
		t.Fatal("Snapshot().SessionID is empty")
	}
}

func TestState_NilSafe(t *testing.T) {
	var s *State
	if got := s.Apply(nil); got.Changed() {
		t.Fatal("Apply on a nil state reported changes")
	}
	if _, ok := s.ParticipantForLabel("1"); ok {
		t.Fatal("ParticipantForLabel on a nil state = true, want false")
	}
	if got := s.Snapshot(); len(got.Participants) != 0 {
		t.Fatal("Snapshot on a nil state returned participants")
	}
	s.SetRaw([]byte("x"))
}

func TestParticipantInfo_Label(t *testing.T) {
	tests := []struct {
		name string
		in   ParticipantInfo
		want string
	}{
		{"name wins", ParticipantInfo{ID: "p", AOR: "sip:a@b", Name: "Alice"}, "Alice"},
		{"aor next", ParticipantInfo{ID: "p", AOR: "sip:a@b"}, "sip:a@b"},
		{"id last", ParticipantInfo{ID: "p"}, "p"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.Label(); got != tc.want {
				t.Fatalf("Label() = %q, want %q", got, tc.want)
			}
		})
	}
}
