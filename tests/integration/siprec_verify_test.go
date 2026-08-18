//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/siprec"
)

// mislabelledMetadata binds participant B to a label the offer never carries.
// The document is valid and self-consistent; only the SDP contradicts it.
func mislabelledMetadata(t *testing.T) []byte {
	t.Helper()
	rec := &siprec.Recording{
		DataMode:               siprec.DataModeComplete,
		SessionRecordingAssocs: []siprec.SessionRecordingAssoc{{SessionID: "s1"}},
		Participants: []siprec.Participant{
			{ParticipantID: "pa", NameIDs: []siprec.NameID{{AOR: "sip:alice@example.com"}}},
			{ParticipantID: "pb", NameIDs: []siprec.NameID{{AOR: "sip:bob@example.com"}}},
		},
		Streams: []siprec.Stream{
			{StreamID: "ta", Label: "1"},
			{StreamID: "tb", Label: "9"},
		},
		ParticipantStreams: []siprec.ParticipantStreamAssoc{
			{ParticipantID: "pa", Send: []string{"ta"}},
			{ParticipantID: "pb", Send: []string{"tb"}},
		},
	}
	md, err := rec.Marshal()
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	return md
}

func siprecLegID(t *testing.T, srs *testInstance) string {
	t.Helper()
	evt := srs.collector.waitForMatch(t, events.SIPRECSessionStarted, nil, 5*time.Second)
	getter, _ := evt.Data.(interface{ GetLegID() string })
	if getter == nil || getter.GetLegID() == "" {
		t.Fatal("siprec.session_started carries no leg ID")
	}
	return getter.GetLegID()
}

func TestSIPREC_MetadataAgreeingWithOfferIsNotFlagged(t *testing.T) {
	src := newTestInstance(t, "src")
	srs := siprecInstance(t, "srs", nil)

	call, err := dialSIPREC(t, src, srs, twoPartyMetadata(t))
	if err != nil {
		t.Fatalf("SIPREC INVITE failed: %v", err)
	}
	defer call.Dialog.Bye(context.Background())

	view := getSIPRECSession(t, srs, siprecLegID(t, srs))
	if len(view.Warnings) != 0 {
		t.Fatalf("metadata matching the offer was flagged: %v", view.Warnings)
	}
}

// A document that contradicts its own SDP is still answered and recorded, but
// the disagreement has to be visible.
func TestSIPREC_MetadataDisagreeingWithOfferIsFlagged(t *testing.T) {
	src := newTestInstance(t, "src")
	srs := siprecInstance(t, "srs", nil)

	call, err := dialSIPREC(t, src, srs, mislabelledMetadata(t))
	if err != nil {
		t.Fatalf("SIPREC INVITE failed: %v", err)
	}
	defer call.Dialog.Bye(context.Background())

	if call.RemoteSDP == nil || len(call.RemoteSDP.Audio) != 2 {
		t.Fatal("the session must still be answered on both streams")
	}

	// One warning, not two: the offer's label 2 is a section this document says
	// nothing about, which is also what a party who has left leaves behind, so
	// it is not reported. Binding bob to a label the offer never carries is.
	view := getSIPRECSession(t, srs, siprecLegID(t, srs))
	if len(view.Warnings) != 1 {
		t.Fatalf("got %d warnings, want 1: %v", len(view.Warnings), view.Warnings)
	}
	if want := string(siprec.IssueUnknownLabel) + " (label 9)"; !strings.Contains(view.Warnings[0], want) {
		t.Errorf("warnings do not mention %q: %v", want, view.Warnings)
	}

	// Flagged, not rejected: the participants are still bound and exposed.
	if len(view.Participants) != 2 {
		t.Errorf("participants = %d, want 2 — a flagged session is still recorded", len(view.Participants))
	}
}

// A re-INVITE can re-offer the media and carry only a delta of the metadata. If
// the new metadata is checked against the sections of the original offer, the
// section this offer adds looks unknown; if the delta is checked instead of the
// accumulated session, the streams it does not mention look unclaimed. Neither
// is a real disagreement.
func TestSIPREC_ReInviteWithNewSectionIsNotFlagged(t *testing.T) {
	src := newTestInstance(t, "src")
	srs := siprecInstance(t, "srs", nil)

	call, err := dialSIPREC(t, src, srs, twoPartyMetadata(t))
	if err != nil {
		t.Fatalf("SIPREC INVITE failed: %v", err)
	}
	defer call.Dialog.Bye(context.Background())

	legID := siprecLegID(t, srs)
	if view := getSIPRECSession(t, srs, legID); len(view.Warnings) != 0 {
		t.Fatalf("the initial session was flagged: %v", view.Warnings)
	}

	// A third party joins: one more sendonly section, and a partial document
	// naming only the newcomer.
	reInviteWithMetadata(t, src, call, srcReofferSDP(call), threePartyPartialJoin(t))
	srs.collector.waitForMatch(t, events.SIPRECMetadataUpdated, nil, 5*time.Second)

	view := getSIPRECSession(t, srs, legID)
	if len(view.Warnings) != 0 {
		t.Fatalf("re-offer plus partial metadata was flagged: %v", view.Warnings)
	}
	if len(view.Participants) != 3 {
		t.Fatalf("participants = %d, want 3 after the join", len(view.Participants))
	}
}

// A metadata-only re-INVITE carries no SDP, so the sections of the established
// offer are still the ones to check against and must not be discarded.
func TestSIPREC_MetadataOnlyReInviteKeepsTheOfferSections(t *testing.T) {
	src := newTestInstance(t, "src")
	srs := siprecInstance(t, "srs", nil)

	call, err := dialSIPREC(t, src, srs, twoPartyMetadata(t))
	if err != nil {
		t.Fatalf("SIPREC INVITE failed: %v", err)
	}
	defer call.Dialog.Bye(context.Background())

	legID := siprecLegID(t, srs)

	rename := &siprec.Recording{
		DataMode: siprec.DataModePartial,
		Participants: []siprec.Participant{{
			ParticipantID: "pa",
			NameIDs:       []siprec.NameID{{AOR: "sip:alice@example.com", Name: &siprec.Name{Value: "Alice Smith"}}},
		}},
	}
	md, err := rename.Marshal()
	if err != nil {
		t.Fatalf("marshal rename: %v", err)
	}

	reInviteWithMetadata(t, src, call, nil, md)
	srs.collector.waitForMatch(t, events.SIPRECMetadataUpdated, nil, 5*time.Second)

	view := getSIPRECSession(t, srs, legID)
	if len(view.Warnings) != 0 {
		t.Fatalf("metadata-only update was flagged: %v", view.Warnings)
	}
	if len(view.Streams) != 2 {
		t.Fatalf("streams = %d, want 2 still bound", len(view.Streams))
	}
}
