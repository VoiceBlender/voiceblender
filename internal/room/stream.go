package room

import (
	"io"
	"sort"
	"strings"

	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/VoiceBlender/voiceblender/internal/mixer"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
)

// streamParticipantSep separates a leg ID from a stream ID in the mixer
// participant namespace. It appears in no other participant ID shape (UUIDs,
// "ws-", "agent-", "tts-", "__bridge:"), so a leg's primary stream keeps the
// bare leg ID and every existing lookup is untouched.
const streamParticipantSep = "#"

// StreamParticipantID is the mixer participant ID for one of a leg's secondary
// audio streams.
func StreamParticipantID(legID, streamID string) string {
	return legID + streamParticipantSep + streamID
}

// SplitStreamParticipantID reports whether id addresses a leg's secondary
// stream, and if so returns the leg and stream IDs.
func SplitStreamParticipantID(id string) (legID, streamID string, ok bool) {
	legID, streamID, ok = strings.Cut(id, streamParticipantSep)
	if !ok || legID == "" || streamID == "" {
		return "", "", false
	}
	return legID, streamID, true
}

// StreamedLeg is a leg whose media is split across several negotiated streams.
type StreamedLeg interface {
	leg.Leg
	StreamMedia(streamID string) (leg.StreamMedia, bool)
	SetStreamRoom(streamID, roomID string)
	SetStreamRole(streamID, role string)
	StreamRooms() map[string]string
}

// legStream is one of a leg's secondary audio streams mixed into this room.
// The leg reference is held directly because the stream's leg need not be a
// participant of this room — that is the whole point of a cross-room stream.
type legStream struct {
	leg      StreamedLeg
	legID    string
	streamID string
	role     string
	part     *mixer.Participant
}

// eofReader is the ingress side of a stream that only transmits. The mixer's
// read loop exits on the first read, which is what "this participant
// contributes no audio" means there.
type eofReader struct{}

func (eofReader) Read([]byte) (int, error) { return 0, io.EOF }

// AddLegStream mixes one of a leg's secondary audio streams into this room,
// returning the mixer participant carrying it.
//
// A stream may join a different room than its leg — that is the point for the
// translation topology, where the original audio and the translated feed are
// mixed separately.
func (r *Room) AddLegStream(l StreamedLeg, streamID, role string) (*mixer.Participant, bool) {
	sm, ok := l.StreamMedia(streamID)
	if !ok {
		return nil, false
	}
	// The primary stream normally joins with its leg, via AddLeg. It may only
	// join as a stream when the leg itself is not a participant here — the case
	// for a recording session, whose m-line 0 is just another party's audio and
	// which must never be handed to the mixer as a whole leg.
	if sm.Primary {
		r.mu.RLock()
		_, isParticipant := r.participants[l.ID()]
		r.mu.RUnlock()
		if isParticipant {
			return nil, false
		}
	}

	reader, writer := streamEndpoints(sm)
	if reader == nil && writer == nil {
		// Inactive: negotiated, but carrying nothing in either direction.
		return nil, false
	}

	pid := StreamParticipantID(l.ID(), streamID)

	r.mu.Lock()
	defer r.mu.Unlock()

	// The stream's own codec decides its rate — two streams on one leg can
	// negotiate different codecs, so the leg's rate is the wrong question.
	rate := sm.SampleRate
	if rate <= 0 {
		rate = r.SampleRate
	}
	p := r.mix.AddParticipant(pid,
		mixer.NewResampleReader(reader, rate, r.SampleRate),
		mixer.NewResampleWriter(writer, r.SampleRate, rate),
	)
	p.MarkOwnerClosesEgress()

	switch sm.Direction {
	case sipmod.DirSendOnly:
		// We only transmit on this stream, so it contributes nothing to the mix.
		r.mix.SetParticipantMuted(pid, true)
	case sipmod.DirRecvOnly:
		// We only receive, so it needs no mixed-minus-self output.
		r.mix.SetParticipantDeaf(pid, true)
	}

	r.legStreams[pid] = &legStream{
		leg:      l,
		legID:    l.ID(),
		streamID: streamID,
		role:     role,
		part:     p,
	}
	l.SetStreamRoom(streamID, r.ID)
	l.SetStreamRole(streamID, role)

	r.applyRoutingLocked()
	r.syncMixerLocked()
	return p, true
}

// StreamParticipant describes one secondary stream mixed into a room, for
// callers that act per audio source rather than per leg — a recording session's
// room holds only these.
type StreamParticipant struct {
	// ParticipantID is the mixer's key for this stream, "legID#streamID".
	ParticipantID string
	LegID         string
	StreamID      string
	// Role is what the stream was attached as — for a recording session the AOR
	// of the party it carries. A metadata refresh can change it, so resolve
	// identity from the leg's recording-session view when it matters.
	Role string
}

// StreamParticipants returns the secondary streams mixed into this room,
// ordered by participant ID so repeated calls behave identically.
func (r *Room) StreamParticipants() []StreamParticipant {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]StreamParticipant, 0, len(r.legStreams))
	for pid, ls := range r.legStreams {
		out = append(out, StreamParticipant{
			ParticipantID: pid,
			LegID:         ls.legID,
			StreamID:      ls.streamID,
			Role:          ls.role,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ParticipantID < out[j].ParticipantID })
	return out
}

// streamEndpoints adapts a stream's direction to the mixer's participant
// contract, which cannot take a nil reader or writer.
func streamEndpoints(sm leg.StreamMedia) (io.Reader, io.Writer) {
	if sm.Reader == nil && sm.Writer == nil {
		return nil, nil
	}
	reader := sm.Reader
	if reader == nil {
		reader = eofReader{}
	}
	writer := sm.Writer
	if writer == nil {
		writer = io.Discard
	}
	return reader, writer
}

// RemoveLegStream detaches a secondary stream from this room.
func (r *Room) RemoveLegStream(participantID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removeLegStreamLocked(participantID)
	r.applyRoutingLocked()
	r.syncMixerLocked()
}

func (r *Room) removeLegStreamLocked(participantID string) {
	ls, ok := r.legStreams[participantID]
	if !ok {
		return
	}
	delete(r.legStreams, participantID)
	r.mix.RemoveParticipant(participantID)
	ls.leg.SetStreamRoom(ls.streamID, "")
	ls.leg.SetStreamRole(ls.streamID, "")
}

// RemoveStreamIfParticipant removes a secondary stream only if p is still the
// mixer participant carrying it. The stream half of RemoveLegIfParticipant:
// panic teardown runs asynchronously, so only the pointer identifies the
// instance that died.
func (r *Room) RemoveStreamIfParticipant(participantID string, p *mixer.Participant) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	ls, ok := r.legStreams[participantID]
	if !ok || ls.part != p {
		return false
	}
	r.removeLegStreamLocked(participantID)
	r.applyRoutingLocked()
	r.syncMixerLocked()
	return true
}

// SetLegStreamRole updates a stream's role in the routing matrix.
func (r *Room) SetLegStreamRole(participantID, role string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	ls, ok := r.legStreams[participantID]
	if !ok {
		return false
	}
	ls.role = role
	ls.leg.SetStreamRole(ls.streamID, role)
	r.applyRoutingLocked()
	return true
}

// LegStreamIDs returns the participant IDs of every secondary stream mixed
// into this room.
func (r *Room) LegStreamIDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.legStreams))
	for pid := range r.legStreams {
		out = append(out, pid)
	}
	return out
}
