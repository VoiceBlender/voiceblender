package room

import (
	"io"
	"log/slog"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/leg"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
)

func TestStreamParticipantID_RoundTrip(t *testing.T) {
	pid := StreamParticipantID("leg-abc", "1")
	if pid != "leg-abc#1" {
		t.Errorf("StreamParticipantID = %q, want leg-abc#1", pid)
	}
	legID, streamID, ok := SplitStreamParticipantID(pid)
	if !ok || legID != "leg-abc" || streamID != "1" {
		t.Errorf("split = (%q, %q, %v)", legID, streamID, ok)
	}
}

func TestSplitStreamParticipantID_LeavesOtherNamespacesAlone(t *testing.T) {
	// Every existing participant ID shape must stay unrecognised, or the panic
	// dispatch would route it to the stream teardown.
	for _, id := range []string{
		"550e8400-e29b-41d4-a716-446655440000",
		"__bridge:abc",
		"ws-deadbeef",
		"agent-deadbeef",
		"tts-deadbeef",
		"",
		"#",
		"leg#",
		"#stream",
	} {
		if _, _, ok := SplitStreamParticipantID(id); ok {
			t.Errorf("SplitStreamParticipantID(%q) matched; it must not", id)
		}
	}
}

// streamMockLeg extends mockLeg with the secondary-stream accessors a room
// needs to mix streams separately from their leg.
type streamMockLeg struct {
	*mockLeg
	streams map[string]leg.StreamMedia
	rooms   map[string]string
}

func newStreamMockLeg(id string, sm leg.StreamMedia) *streamMockLeg {
	return &streamMockLeg{
		mockLeg: newAudioMockLeg(id),
		streams: map[string]leg.StreamMedia{sm.ID: sm},
		rooms:   map[string]string{},
	}
}

func (f *streamMockLeg) StreamMedia(streamID string) (leg.StreamMedia, bool) {
	sm, ok := f.streams[streamID]
	return sm, ok
}

func (f *streamMockLeg) SetStreamRoom(streamID, roomID string) {
	f.rooms[streamID] = roomID
}

func (f *streamMockLeg) SetStreamRole(string, string) {}

func sendonlyStream(id string, rate int) leg.StreamMedia {
	return leg.StreamMedia{
		ID:         id,
		MID:        id,
		SampleRate: rate,
		Direction:  sipmod.DirSendOnly,
		Writer:     io.Discard,
	}
}

func TestAddLegStream_UsesParticipantIDAndStreamRate(t *testing.T) {
	r := NewRoom("room-1", "", 16000, slog.New(slog.DiscardHandler))
	// The stream's own codec runs at 8 kHz while the room mixes at 16 kHz —
	// the stream's rate must drive the resampler, not the leg's.
	l := newStreamMockLeg("leg-1", sendonlyStream("1", 8000))

	p, ok := r.AddLegStream(l, "1", "translator")
	if !ok {
		t.Fatal("AddLegStream failed")
	}
	if p.ID != "leg-1#1" {
		t.Errorf("participant ID = %q, want leg-1#1", p.ID)
	}
	if got := l.rooms["1"]; got != "room-1" {
		t.Errorf("stream room = %q, want room-1", got)
	}
	if !r.mixerShouldRun() {
		t.Error("a room holding only a stream must still run its mixer")
	}
	if len(r.LegStreamIDs()) != 1 {
		t.Errorf("LegStreamIDs = %v, want one entry", r.LegStreamIDs())
	}
}

func TestAddLegStream_SendonlyIsMuted(t *testing.T) {
	r := NewRoom("room-1", "", 16000, slog.New(slog.DiscardHandler))
	l := newStreamMockLeg("leg-1", sendonlyStream("1", 16000))

	p, ok := r.AddLegStream(l, "1", "")
	if !ok {
		t.Fatal("AddLegStream failed")
	}
	// We only transmit on this stream, so it must contribute nothing to the mix.
	if !p.Muted.Load() {
		t.Error("a sendonly stream must be muted as a source")
	}
}

func TestAddLegStream_RecvonlyIsDeaf(t *testing.T) {
	r := NewRoom("room-1", "", 16000, slog.New(slog.DiscardHandler))
	sm := leg.StreamMedia{
		ID: "1", MID: "1", SampleRate: 16000,
		Direction: sipmod.DirRecvOnly,
		Reader:    eofReader{},
	}
	l := newStreamMockLeg("leg-1", sm)

	p, ok := r.AddLegStream(l, "1", "")
	if !ok {
		t.Fatal("AddLegStream failed")
	}
	if !p.Deaf.Load() {
		t.Error("a recvonly stream needs no mixed-minus-self output, so it must be deaf")
	}
}

func TestAddLegStream_RejectsPrimaryAndInactive(t *testing.T) {
	r := NewRoom("room-1", "", 16000, slog.New(slog.DiscardHandler))

	primary := newStreamMockLeg("leg-1", leg.StreamMedia{ID: "0", Primary: true, Writer: io.Discard})
	if _, ok := r.AddLegStream(primary, "0", ""); ok {
		t.Error("the primary stream joins via AddLeg, not AddLegStream")
	}

	inactive := newStreamMockLeg("leg-2", leg.StreamMedia{ID: "1", Direction: sipmod.DirInactive})
	if _, ok := r.AddLegStream(inactive, "1", ""); ok {
		t.Error("an inactive stream carries nothing and must not join the mixer")
	}
}

func TestRemoveLegStream_ClearsRoomAndStopsMixer(t *testing.T) {
	r := NewRoom("room-1", "", 16000, slog.New(slog.DiscardHandler))
	l := newStreamMockLeg("leg-1", sendonlyStream("1", 16000))
	r.AddLegStream(l, "1", "")

	r.RemoveLegStream("leg-1#1")

	if len(r.LegStreamIDs()) != 0 {
		t.Error("stream was not removed")
	}
	if got := l.rooms["1"]; got != "" {
		t.Errorf("stream room = %q, want cleared", got)
	}
	if r.mixerShouldRun() {
		t.Error("an empty room must not keep its mixer running")
	}
}

func TestRemoveStreamIfParticipant_PointerElection(t *testing.T) {
	r := NewRoom("room-1", "", 16000, slog.New(slog.DiscardHandler))
	l := newStreamMockLeg("leg-1", sendonlyStream("1", 16000))
	first, _ := r.AddLegStream(l, "1", "")

	// The stream is replaced by a fresh participant before the panic teardown
	// for the old one runs; the stale pointer must not evict the live instance.
	r.RemoveLegStream("leg-1#1")
	second, _ := r.AddLegStream(l, "1", "")

	if r.RemoveStreamIfParticipant("leg-1#1", first) {
		t.Error("a stale participant must not remove its successor")
	}
	if len(r.LegStreamIDs()) != 1 {
		t.Fatal("the live stream was evicted")
	}
	if !r.RemoveStreamIfParticipant("leg-1#1", second) {
		t.Error("the live participant must be removable")
	}
}

func TestRemoveLeg_TakesItsStreamsInThisRoom(t *testing.T) {
	r := NewRoom("room-1", "", 16000, slog.New(slog.DiscardHandler))
	l := newStreamMockLeg("leg-1", sendonlyStream("1", 16000))
	r.AddLeg(l)
	r.AddLegStream(l, "1", "")

	r.RemoveLeg("leg-1")

	if len(r.LegStreamIDs()) != 0 {
		t.Errorf("leg removal must take its streams in this room: %v", r.LegStreamIDs())
	}
}

// TestRouting_SiblingStreamsDoNotHearEachOther pins the rule that makes the
// translation topology work: without it the original stream hears the
// translated one and echoes it straight back to the peer.
func TestRouting_SiblingStreamsDoNotHearEachOther(t *testing.T) {
	r := NewRoom("room-1", "", 16000, slog.New(slog.DiscardHandler))
	caller := newStreamMockLeg("leg-1", sendonlyStream("1", 16000))
	other := newAudioMockLeg("leg-2")

	r.AddLeg(caller)
	r.AddLeg(other)
	r.AddLegStream(caller, "1", "")

	hears, _ := r.mix.ParticipantHears("leg-1")
	if hears == nil {
		t.Fatal("a leg with a sibling stream must get an explicit hears set, not full mesh")
	}
	if _, ok := hears["leg-1#1"]; ok {
		t.Error("a leg must not hear its own secondary stream")
	}
	if _, ok := hears["leg-2"]; !ok {
		t.Error("the leg must still hear the other participant")
	}

	// The stream itself must not hear its owning leg either.
	streamHears, _ := r.mix.ParticipantHears("leg-1#1")
	if streamHears == nil {
		t.Fatal("the stream must get an explicit hears set")
	}
	if _, ok := streamHears["leg-1"]; ok {
		t.Error("a stream must not hear its own leg")
	}

	// An unrelated leg keeps full-mesh routing.
	if h, _ := r.mix.ParticipantHears("leg-2"); h != nil {
		t.Error("a leg with no sibling streams should stay full mesh")
	}
}

func TestRouting_StreamsCarryTheirOwnRole(t *testing.T) {
	r := NewRoom("room-1", "", 16000, slog.New(slog.DiscardHandler))
	listener := newAudioMockLeg("leg-listener")
	listener.SetRole("listener")
	owner := newStreamMockLeg("leg-owner", sendonlyStream("1", 16000))
	owner.SetRole("speaker")

	r.AddLeg(listener)
	r.AddLeg(owner)
	r.AddLegStream(owner, "1", "translator")

	// The listener only hears "translator", so it must get the stream and not
	// the leg carrying it.
	r.SetRoutingMatrix(map[string][]string{"listener": {"translator"}})

	hears, _ := r.mix.ParticipantHears("leg-listener")
	if hears == nil {
		t.Fatal("a matrixed listener must get an explicit hears set")
	}
	if _, ok := hears["leg-owner#1"]; !ok {
		t.Error("the listener must hear the translator stream")
	}
	if _, ok := hears["leg-owner"]; ok {
		t.Error("the listener must not hear the speaker leg")
	}

	if !r.SetLegStreamRole("leg-owner#1", "speaker") {
		t.Fatal("SetLegStreamRole failed")
	}
	after, _ := r.mix.ParticipantHears("leg-listener")
	if _, ok := after["leg-owner#1"]; ok {
		t.Error("after the role change the stream must drop out of the listener's mix")
	}
}
