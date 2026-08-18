package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/VoiceBlender/voiceblender/internal/room"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
	"github.com/VoiceBlender/voiceblender/internal/siprec"
	"github.com/emiago/sipgo/sip"
	"github.com/go-chi/chi/v5"
)

// siprecSession is the recording-session state that lives beside a SIPREC leg.
// The leg, room and mixer layers stay SIPREC-ignorant; the label→participant
// join happens here.
type siprecSession struct {
	state *siprec.State
	// sections is the offer's labelled m= sections, kept so a metadata update
	// can be re-checked against the SDP the session was established with.
	sections []siprec.MediaSection
	// roomID is the room this session's streams were auto-attached to, and
	// ownsRoom records whether we created it and must delete it on teardown.
	roomID   string
	ownsRoom bool
}

func (s *Server) putSIPRECSession(legID string, sess *siprecSession) {
	s.siprecMu.Lock()
	defer s.siprecMu.Unlock()
	s.siprecSessions[legID] = sess
}

func (s *Server) siprecSession(legID string) (*siprecSession, bool) {
	s.siprecMu.Lock()
	defer s.siprecMu.Unlock()
	sess, ok := s.siprecSessions[legID]
	return sess, ok
}

func (s *Server) dropSIPRECSession(legID string) (*siprecSession, bool) {
	s.siprecMu.Lock()
	defer s.siprecMu.Unlock()
	sess, ok := s.siprecSessions[legID]
	delete(s.siprecSessions, legID)
	return sess, ok
}

// ClaimSIPREC reports whether an inbound INVITE was taken over as a recording
// session. A false return means the INVITE is an ordinary call and the normal
// path must continue handling it.
//
// With SIPREC disabled only Require/Proxy-Require is answered, because only an
// option tag obliges us to respond at all (RFC 3261 §8.2.2.3). A +sip.src
// Contact hint or a stray metadata part does not, so an INVITE carrying one of
// those keeps the behaviour it had before SIPREC existed rather than being
// refused by a server that was never asked to be a recording server.
func (s *Server) ClaimSIPREC(call *sipmod.InboundCall) bool {
	signals := sipmod.DetectSIPREC(call)
	if !signals.Claimed() {
		return false
	}
	if !s.Config.SIPRECEnabled {
		if !signals.Required {
			return false
		}
		s.rejectSIPREC(call, signals)
		return true
	}
	s.HandleSIPRECInbound(call, signals)
	return true
}

// HandleSIPRECInbound answers an inbound SIPREC recording session (RFC 7866).
// It mirrors HandleInboundCall but with recording-session policy: no ringing,
// answer without waiting for the API, and never transmit.
func (s *Server) HandleSIPRECInbound(call *sipmod.InboundCall, signals sipmod.SIPRECSignals) {
	// A claimed recording session with no metadata cannot be bound to
	// participants, which is the entire point of accepting it.
	md, ok := call.Body.RSMetadata()
	if !ok {
		s.Log.Warn("SIPREC INVITE carries no rs-metadata", "signals", fmt.Sprintf("%+v", signals))
		s.respondSIPREC(call, sip.StatusBadRequest, "Missing Recording Metadata")
		return
	}
	if max := s.Config.SIPRECMetadataMaxBytes; max > 0 && len(md) > max {
		s.Log.Warn("SIPREC metadata too large", "bytes", len(md), "max", max)
		s.respondSIPREC(call, http.StatusRequestEntityTooLarge, "Metadata Too Large")
		return
	}

	rec, err := siprec.Parse(md)
	if err != nil {
		s.Log.Warn("parse SIPREC metadata failed", "error", err)
		s.respondSIPREC(call, sip.StatusBadRequest, "Bad Recording Metadata")
		return
	}

	if max := s.Config.SIPRECMaxStreams; max > 0 && call.RemoteSDP != nil && len(call.RemoteSDP.Audio) > max {
		s.Log.Warn("SIPREC session offers too many streams",
			"streams", len(call.RemoteSDP.Audio), "max", max)
		s.respondSIPREC(call, sip.StatusBusyHere, "Too Many Streams")
		return
	}

	if err := s.SIPEngine.DialogRespond(call.Dialog, sip.StatusTrying, "Trying", nil, s.SIPEngine.ServerHeader()); err != nil {
		s.Log.Error("SIPREC: failed to send 100 Trying", "error", err)
		return
	}

	sess := &siprecSession{state: siprec.NewState(), sections: mediaSections(call.RemoteSDP)}
	sess.state.Apply(rec)
	sess.state.SetRaw(md)

	l := leg.NewSIPRECInboundLeg(call, s.SIPEngine, s.Log)
	// After the leg exists, so a warning carries the leg it came from.
	s.verifySIPRECMetadata(sess, l.ID())
	if appID, ok := l.SIPHeaders()["X-App-ID"]; ok {
		l.SetAppID(appID)
	}
	s.LegMgr.Add(l)
	s.putSIPRECSession(l.ID(), sess)
	l.SetJitterBuffer(s.Config.SIPJitterBufferMs, s.Config.SIPJitterBufferMaxMs)
	s.applyLegWebhook(l, call)

	s.Bus.Publish(events.LegRinging, &events.LegRingingData{
		LegScope:      events.LegScope{LegID: l.ID(), AppID: l.AppID()},
		LegType:       string(l.Type()),
		From:          call.From,
		To:            call.To,
		SIPHeaders:    l.SIPHeaders(),
		OfferedCodecs: buildOfferedCodecs(call.RemoteSDP),
		SourceAddress: call.Request.Source(),
	})

	if !s.Config.SIPRECAutoAnswer {
		select {
		case <-l.AnswerCh():
		case <-call.Dialog.Context().Done():
			if l.State() != leg.StateHungUp {
				s.cleanupLeg(l)
				s.publishDisconnect(l, "caller_cancel")
			}
			return
		}
	}

	if err := l.Answer(context.Background()); err != nil {
		s.Log.Error("SIPREC: answer failed", "leg_id", l.ID(), "error", err)
		s.cleanupLeg(l)
		s.publishDisconnect(l, "siprec_answer_failed")
		return
	}

	s.setupLegEventForwarding(l)
	s.Bus.Publish(events.LegConnected, &events.LegConnectedData{
		LegScope: events.LegScope{LegID: l.ID(), AppID: l.AppID()},
		LegType:  string(l.Type()),
	})
	s.publishSIPRECStarted(l, sess)
	s.attachSIPRECRooms(l, sess)
	s.maybeAutoRecordSIPREC(l)

	s.watchLegDialogEnd(l, call.Dialog.Context(), 0)
}

// rejectSIPREC declines a recording session this server will not accept. The
// peer demanded the siprec extension, so it gets 420 with Unsupported and
// learns why, rather than a bare failure it would retry.
func (s *Server) rejectSIPREC(call *sipmod.InboundCall, signals sipmod.SIPRECSignals) {
	res := sip.NewResponseFromRequest(call.Request, sip.StatusBadExtension, "Bad Extension", nil)
	res.AppendHeader(sipmod.UnsupportedHeader(sipmod.OptionTagSIPREC))
	res.AppendHeader(s.SIPEngine.ServerHeader())
	res.AppendHeader(s.SIPEngine.AllowHeader())
	if err := call.Dialog.WriteResponse(res); err != nil {
		s.Log.Error("SIPREC: respond 420 failed", "error", err)
	}
}

func (s *Server) respondSIPREC(call *sipmod.InboundCall, code int, reason string) {
	if err := s.SIPEngine.DialogRespond(call.Dialog, code, reason, nil, s.SIPEngine.ServerHeader()); err != nil {
		s.Log.Error("SIPREC: respond failed", "code", code, "error", err)
	}
}

// mediaSections reduces an offer to the labelled sections a metadata document
// can be checked against.
func mediaSections(sdp *sipmod.SDPMedia) []siprec.MediaSection {
	if sdp == nil {
		return nil
	}
	out := make([]siprec.MediaSection, 0, len(sdp.Audio))
	for i := range sdp.Audio {
		// An unlabelled section binds to no <stream> element.
		if sdp.Audio[i].Label == "" {
			continue
		}
		out = append(out, siprec.MediaSection{
			Label: sdp.Audio[i].Label,
			CNAME: sdp.Audio[i].CNAME,
		})
	}
	return out
}

// verifySIPRECMetadata records what the offer disproves about the document. The
// session is never rejected over it: which party is on which label is the
// recording client's statement to make, and it cannot always be disproved.
func (s *Server) verifySIPRECMetadata(sess *siprecSession, legID string) {
	// The merged session, not the document just applied: a partial update would
	// report every stream it does not mention as unclaimed.
	issues := siprec.Verify(sess.state.Merged(), sess.sections)
	sess.state.SetWarnings(issues)
	for _, issue := range issues {
		s.Log.Warn("SIPREC metadata disagrees with the offer",
			"leg_id", legID, "kind", string(issue.Kind), "label", issue.Label, "detail", issue.Detail)
	}
}

// siprecStreams joins the leg's negotiated streams to the metadata by a=label.
func (s *Server) siprecStreams(sl *leg.SIPLeg, sess *siprecSession) []events.SIPRECStream {
	infos := sl.Streams()
	out := make([]events.SIPRECStream, 0, len(infos))
	for _, info := range infos {
		st := events.SIPRECStream{Label: info.Label, LegStreamID: info.ID}
		if p, ok := sess.state.ParticipantForLabel(info.Label); ok {
			st.ParticipantID, st.ParticipantAOR, st.ParticipantName = p.ID, p.AOR, p.Name
		}
		out = append(out, st)
	}
	return out
}

func (s *Server) publishSIPRECStarted(sl *leg.SIPLeg, sess *siprecSession) {
	snap := sess.state.Snapshot()
	s.Bus.Publish(events.SIPRECSessionStarted, &events.SIPRECSessionStartedData{
		LegScope:     events.LegScope{LegID: sl.ID(), AppID: sl.AppID()},
		SessionID:    snap.SessionID,
		DataMode:     snap.DataMode,
		Participants: snap.Participants,
		Streams:      s.siprecStreams(sl, sess),
	})
}

// attachSIPRECRooms places every stream of the session into a room, when
// configured to. Each stream takes the participant it carries as its routing
// role, so a routing matrix can address recorded parties by identity.
func (s *Server) attachSIPRECRooms(sl *leg.SIPLeg, sess *siprecSession) {
	roomID, ownsRoom := "", false

	switch s.Config.SIPRECRoomMode {
	case config.SIPRECRoomModePerSession:
		roomID = "siprec-" + sl.ID()
		if _, err := s.RoomMgr.Create(roomID, sl.AppID(), s.Config.DefaultSampleRate); err != nil {
			s.Log.Warn("SIPREC: create session room failed", "leg_id", sl.ID(), "room_id", roomID, "error", err)
			return
		}
		ownsRoom = true
	case config.SIPRECRoomModeFixed:
		roomID = s.Config.SIPRECRoomID
		if roomID == "" {
			s.Log.Warn("SIPREC_ROOM_MODE=fixed but SIPREC_ROOM_ID is empty; not attaching", "leg_id", sl.ID())
			return
		}
	default:
		return
	}

	rm, ok := s.RoomMgr.Get(roomID)
	if !ok {
		s.Log.Warn("SIPREC: room not found", "leg_id", sl.ID(), "room_id", roomID)
		return
	}

	attached := 0
	for _, info := range sl.Streams() {
		role := ""
		if p, ok := sess.state.ParticipantForLabel(info.Label); ok {
			role = p.Label()
		}
		if _, ok := rm.AddLegStream(sl, info.ID, role); !ok {
			s.Log.Warn("SIPREC: attach stream to room failed",
				"leg_id", sl.ID(), "stream_id", info.ID, "room_id", roomID)
			continue
		}
		attached++
	}

	if attached == 0 && ownsRoom {
		s.RoomMgr.Delete(roomID)
		return
	}

	s.siprecMu.Lock()
	sess.roomID, sess.ownsRoom = roomID, ownsRoom
	s.siprecMu.Unlock()
}

// applySIPRECMetadata merges a metadata document arriving on a re-INVITE or
// UPDATE into the session state and publishes what changed. It is a no-op for
// any leg that is not a recording session, and for a body carrying no metadata.
//
// The SDP must already have been applied: a participant joining brings a new
// m= section, and the label→participant join only resolves once that section's
// stream exists.
func (s *Server) applySIPRECMetadata(sl *leg.SIPLeg, body *sipmod.MessageBody) {
	if sl == nil || !sl.Type().IsSIPREC() {
		return
	}
	md, ok := body.RSMetadata()
	if !ok {
		return
	}
	sess, ok := s.siprecSession(sl.ID())
	if !ok {
		return
	}
	if max := s.Config.SIPRECMetadataMaxBytes; max > 0 && len(md) > max {
		s.Log.Warn("SIPREC metadata update too large; ignoring",
			"leg_id", sl.ID(), "bytes", len(md), "max", max)
		return
	}

	rec, err := siprec.Parse(md)
	if err != nil {
		s.Log.Warn("parse SIPREC metadata update failed", "leg_id", sl.ID(), "error", err)
		return
	}

	before := sess.state.Snapshot()
	delta := sess.state.Apply(rec)
	sess.state.SetRaw(md)

	// A re-INVITE may re-offer the media too; checking against the original
	// offer's sections would report the new labels as unknown.
	if sdp, ok := body.SDP(); ok {
		if parsed, err := sipmod.ParseSDP(sdp); err == nil {
			sess.sections = mediaSections(parsed)
		} else {
			s.Log.Warn("SIPREC: re-offer SDP did not parse; keeping the previous sections",
				"leg_id", sl.ID(), "error", err)
		}
	}
	s.verifySIPRECMetadata(sess, sl.ID())

	snap := sess.state.Snapshot()
	scope := events.LegScope{LegID: sl.ID(), AppID: sl.AppID()}
	streams := s.siprecStreams(sl, sess)

	s.Bus.Publish(events.SIPRECMetadataUpdated, &events.SIPRECMetadataUpdatedData{
		LegScope:           scope,
		SessionID:          snap.SessionID,
		DataMode:           snap.DataMode,
		ParticipantsJoined: delta.ParticipantsAdded,
		ParticipantsLeft:   delta.ParticipantsRemoved,
		StreamsAdded:       delta.StreamsAdded,
		StreamsRemoved:     delta.StreamsRemoved,
		Streams:            streams,
	})

	for _, id := range delta.ParticipantsAdded {
		s.Bus.Publish(events.SIPRECParticipantJoined, &events.SIPRECParticipantJoinedData{
			LegScope:     scope,
			SessionID:    snap.SessionID,
			SIPRECStream: siprecStreamForParticipant(streams, id),
		})
	}
	for _, id := range delta.ParticipantsRemoved {
		s.Bus.Publish(events.SIPRECParticipantLeft, &events.SIPRECParticipantLeftData{
			LegScope:     scope,
			SessionID:    snap.SessionID,
			SIPRECStream: siprecStreamForParticipant(streamsBefore(before, id), id),
		})
	}

	if delta.Changed() {
		s.syncSIPRECRooms(sl, sess)
	}
}

// siprecStreamForParticipant finds the stream a participant sends on, so a
// join/leave event names the media it affects. Returns a bare identity when the
// participant has no stream bound.
func siprecStreamForParticipant(streams []events.SIPRECStream, participantID string) events.SIPRECStream {
	for _, st := range streams {
		if st.ParticipantID == participantID {
			return st
		}
	}
	return events.SIPRECStream{ParticipantID: participantID}
}

// streamsBefore renders a departed participant's stream from the pre-update
// snapshot, since it is gone from the current one.
func streamsBefore(before siprec.Snapshot, participantID string) []events.SIPRECStream {
	var out []events.SIPRECStream
	for _, st := range before.Streams {
		if st.Participant.ID != participantID {
			continue
		}
		out = append(out, events.SIPRECStream{
			Label:           st.Label,
			ParticipantID:   st.Participant.ID,
			ParticipantAOR:  st.Participant.AOR,
			ParticipantName: st.Participant.Name,
		})
	}
	return out
}

// syncSIPRECRooms attaches streams that appeared since the session was set up
// and re-roles the ones whose participant changed. Streams whose section the
// peer disabled are already gone: closing a stream removes it from its room.
func (s *Server) syncSIPRECRooms(sl *leg.SIPLeg, sess *siprecSession) {
	s.siprecMu.Lock()
	roomID := sess.roomID
	s.siprecMu.Unlock()
	if roomID == "" {
		return
	}
	rm, ok := s.RoomMgr.Get(roomID)
	if !ok {
		return
	}

	for _, info := range sl.Streams() {
		role := ""
		if p, ok := sess.state.ParticipantForLabel(info.Label); ok {
			role = p.Label()
		}
		if info.RoomID == roomID {
			if role != info.Role {
				rm.SetLegStreamRole(room.StreamParticipantID(sl.ID(), info.ID), role)
			}
			continue
		}
		if _, ok := rm.AddLegStream(sl, info.ID, role); !ok {
			s.Log.Warn("SIPREC: attach updated stream to room failed",
				"leg_id", sl.ID(), "stream_id", info.ID, "room_id", roomID)
		}
	}
}

// cleanupSIPRECSession drops a recording session's state and the room we
// created for it. Called from cleanupLeg, so it must tolerate a non-SIPREC leg.
func (s *Server) cleanupSIPRECSession(l leg.Leg) {
	if !l.Type().IsSIPREC() {
		return
	}
	sess, ok := s.dropSIPRECSession(l.ID())
	if !ok {
		return
	}

	s.Bus.Publish(events.SIPRECSessionEnded, &events.SIPRECSessionEndedData{
		LegScope:  events.LegScope{LegID: l.ID(), AppID: l.AppID()},
		SessionID: sess.state.SessionID(),
	})

	// detachLegStreams has already removed the streams; only a room we minted
	// for this session is ours to delete.
	if sess.ownsRoom && sess.roomID != "" {
		s.RoomMgr.Delete(sess.roomID)
	}
}

// --- REST ---

func (s *Server) doGetSIPRECSession(legID string) (*SIPRECSessionView, error) {
	l, ok := s.LegMgr.Get(legID)
	if !ok {
		return nil, newAPIError(http.StatusNotFound, "leg not found")
	}
	sl, ok := l.(*leg.SIPLeg)
	if !ok || !l.Type().IsSIPREC() {
		return nil, newAPIError(http.StatusBadRequest, "leg is not a SIPREC recording session")
	}
	sess, ok := s.siprecSession(legID)
	if !ok {
		return nil, newAPIError(http.StatusNotFound, "recording session state not found")
	}

	snap := sess.state.Snapshot()
	view := &SIPRECSessionView{
		LegID:        legID,
		SessionID:    snap.SessionID,
		DataMode:     snap.DataMode,
		RoomID:       sess.roomID,
		Participants: make([]SIPRECParticipantView, 0, len(snap.Participants)),
		Streams:      make([]SIPRECStreamView, 0, len(snap.Streams)),
		Metadata:     snap.Metadata,
		Warnings:     snap.Warnings,
	}
	for _, p := range snap.Participants {
		view.Participants = append(view.Participants, SIPRECParticipantView{
			ParticipantID: p.ID,
			AOR:           p.AOR,
			Name:          p.Name,
		})
	}
	for _, info := range sl.Streams() {
		sv := SIPRECStreamView{
			LegStreamID: info.ID,
			MID:         info.MID,
			Label:       info.Label,
			Direction:   info.Direction,
			Codec:       info.Codec,
			RoomID:      info.RoomID,
			Role:        info.Role,
		}
		if p, ok := sess.state.ParticipantForLabel(info.Label); ok {
			sv.ParticipantID, sv.ParticipantAOR, sv.ParticipantName = p.ID, p.AOR, p.Name
		}
		view.Streams = append(view.Streams, sv)
	}
	return view, nil
}

func (s *Server) getSIPRECSession(w http.ResponseWriter, r *http.Request) {
	view, err := s.doGetSIPRECSession(chi.URLParam(r, "id"))
	if err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}
