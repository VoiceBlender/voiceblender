package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/VoiceBlender/voiceblender/internal/mixer"
	"github.com/VoiceBlender/voiceblender/internal/room"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
	"github.com/VoiceBlender/voiceblender/internal/siprec"
	"github.com/emiago/sipgo/sip"
	"github.com/go-chi/chi/v5"
)

// srcPump copies one recorded party's own audio into the recording session
// stream carrying it.
//
// The pipe is not optional. Mixer taps are written inline on the mix-tick
// goroutine, while a leg stream's writer blocks until the frame is accepted, so
// wiring the two together directly would stall the whole room's mixer whenever
// RTP pacing slips.
type srcPump struct {
	participantID string
	streamID      string
	pipe          *pipeWriter
	// legID is set when the source is a leg's own in/out taps rather than a
	// mixer participant, so teardown knows which to clear.
	legID string
}

// srcParty is one thing an outbound recording session forks: a room
// participant, one of a leg's secondary streams, or a whole call's own audio.
type srcParty struct {
	// id is both the selector callers use and the participant_id in the
	// metadata: a leg ID, or "legID#streamID" for a secondary stream.
	id  string
	aor string
	// mixerID is the mixer participant to tap. Empty for a leg-level session,
	// which taps the leg directly.
	mixerID string
}

// siprecSRC is an outbound recording session: a siprec_out leg plus the pumps
// feeding each of its streams from one recorded party.
type siprecSRC struct {
	mu     sync.Mutex
	legID  string
	roomID string
	pumps  []*srcPump
}

// doStartRoomSIPREC forks every participant of a room to an external session
// recording server, one sendonly m=audio per party.
func (s *Server) doStartRoomSIPREC(ctx context.Context, roomID string, req StartSIPRECRequest) (*LegView, error) {
	if !s.Config.SIPRECSRCEnabled {
		return nil, newAPIError(http.StatusForbidden, "outbound SIPREC is disabled; set SIPREC_SRC_ENABLED=true")
	}
	recipient, err := srcRecipient(req)
	if err != nil {
		return nil, err
	}

	rm, ok := s.RoomMgr.Get(roomID)
	if !ok {
		return nil, newAPIError(http.StatusNotFound, "room not found")
	}

	parties, unknown := selectSRCParties(s.roomSRCParties(rm), req.LegIDs)
	if len(unknown) > 0 {
		return nil, newAPIError(http.StatusNotFound,
			"not participants of room %q: %s", roomID, strings.Join(unknown, ", "))
	}
	if len(parties) == 0 {
		return nil, newAPIError(http.StatusConflict, "room has no participants to record")
	}
	if max := s.Config.SIPRECMaxStreams; max > 0 && len(parties) > max {
		return nil, newAPIError(http.StatusConflict,
			"selected %d participants, more than SIPREC_MAX_STREAMS (%d)", len(parties), max)
	}

	sessionID := req.SessionID
	if sessionID == "" {
		sessionID = roomID
	}
	call, labels, err := s.inviteSRS(ctx, recipient, sessionID, parties, req)
	if err != nil {
		return nil, err
	}

	l := leg.NewSIPRECOutboundLeg(call, s.SIPEngine, s.Log)
	l.SetAppID(req.AppID)
	s.LegMgr.Add(l)

	src := &siprecSRC{legID: l.ID(), roomID: roomID}
	s.attachSRCPumps(l, rm.Mixer(), parties, labels, src)

	s.siprecMu.Lock()
	s.siprecSRCs[l.ID()] = src
	s.siprecMu.Unlock()

	s.Bus.Publish(events.SIPRECSessionStarted, &events.SIPRECSessionStartedData{
		LegScope:     events.LegScope{LegID: l.ID(), AppID: l.AppID()},
		SessionID:    sessionID,
		DataMode:     siprec.DataModeComplete,
		Participants: srcParticipantInfos(parties),
		Streams:      srcStreamViews(l, parties, labels),
	})

	go s.watchLegDialogEnd(l, call.Dialog.Context(), 0)

	view := s.toLegView(l)
	return &view, nil
}

// roomSRCParties enumerates everything in a room that can be forked: each
// participant leg, plus each secondary stream mixed into the room. A stream may
// belong to a leg that is not itself a participant — that is the whole point of
// a cross-room stream — so both are addressable.
func (s *Server) roomSRCParties(rm *room.Room) []srcParty {
	var out []srcParty

	for _, l := range rm.Participants() {
		// A recording session must not record another recording session.
		if l.Type().IsSIPREC() {
			continue
		}
		out = append(out, srcParty{id: l.ID(), aor: srcParticipantAOR(l), mixerID: l.ID()})
	}

	for _, pid := range rm.LegStreamIDs() {
		legID, streamID, ok := room.SplitStreamParticipantID(pid)
		if !ok {
			continue
		}
		party := srcParty{id: pid, mixerID: pid, aor: "sip:" + legID + "@voiceblender.local"}
		if l, found := s.LegMgr.Get(legID); found {
			party.aor = srcParticipantAOR(l)
			if sl, isSIP := l.(*leg.SIPLeg); isSIP {
				if info, has := sl.Stream(streamID); has && info.Label != "" {
					party.aor = srcStreamAOR(legID, info.Label)
				}
			}
		}
		out = append(out, party)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

// srcStreamAOR names a secondary stream distinctly from its leg's own audio, so
// a recording server sees two different parties rather than one repeated.
func srcStreamAOR(legID, label string) string {
	return "sip:" + legID + "+" + label + "@voiceblender.local"
}

// selectSRCParties narrows the candidates to the caller's allowlist, preserving
// the candidate order so m-line assignment stays deterministic. An empty
// allowlist selects everything.
func selectSRCParties(candidates []srcParty, want []string) ([]srcParty, []string) {
	if len(want) == 0 {
		return candidates, nil
	}
	byID := make(map[string]srcParty, len(candidates))
	for _, c := range candidates {
		byID[c.id] = c
	}

	var (
		out     []srcParty
		unknown []string
		seen    = make(map[string]bool, len(want))
	)
	for _, id := range want {
		c, ok := byID[id]
		if !ok {
			unknown = append(unknown, id)
			continue
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, c)
	}
	// Restore candidate order so the same selection always maps to the same
	// m-lines, whatever order the caller listed them in.
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out, unknown
}

// buildSRCMetadata renders the RFC 7865 document describing the parties we are
// about to fork, returning it alongside the leg-ID→label map the SDP must use.
func buildSRCMetadata(sessionID string, parties []srcParty) (*siprec.Recording, map[string]string) {
	rec := &siprec.Recording{
		DataMode:               siprec.DataModeComplete,
		Sessions:               []siprec.Session{{SessionID: sessionID}},
		SessionRecordingAssocs: []siprec.SessionRecordingAssoc{{SessionID: sessionID}},
	}
	labels := make(map[string]string, len(parties))

	for i, p := range parties {
		label := fmt.Sprintf("%d", i+1)
		streamID := "stream-" + label
		labels[p.id] = label

		rec.Participants = append(rec.Participants, siprec.Participant{
			ParticipantID: p.id,
			SessionID:     sessionID,
			NameIDs:       []siprec.NameID{{AOR: p.aor}},
		})
		rec.ParticipantSessions = append(rec.ParticipantSessions, siprec.ParticipantSessionAssoc{
			ParticipantID: p.id,
			SessionID:     sessionID,
		})
		rec.Streams = append(rec.Streams, siprec.Stream{
			StreamID:  streamID,
			SessionID: sessionID,
			Label:     label,
		})
		rec.ParticipantStreams = append(rec.ParticipantStreams, siprec.ParticipantStreamAssoc{
			ParticipantID: p.id,
			Send:          []string{streamID},
		})
	}
	return rec, labels
}

// srcParticipantAOR names a leg the way a recording server expects: a SIP URI
// when the leg has one, otherwise a synthetic one built from its ID, so every
// participant carries something resolvable.
func srcParticipantAOR(l leg.Leg) string {
	if hdrs := l.SIPHeaders(); hdrs != nil {
		if from, ok := hdrs["X-SIPREC-AOR"]; ok && from != "" {
			return from
		}
	}
	return "sip:" + l.ID() + "@voiceblender.local"
}

// attachSRCPumps wires each recorded party's own audio into the stream carrying
// them. A party whose stream the recording server refused is simply not pumped.
func (s *Server) attachSRCPumps(l *leg.SIPLeg, mix *mixer.Mixer, parties []srcParty, labels map[string]string, src *siprecSRC) {
	byLabel := make(map[string]leg.StreamInfo, len(parties))
	for _, info := range l.Streams() {
		byLabel[info.Label] = info
	}

	for _, party := range parties {
		info, ok := byLabel[labels[party.id]]
		if !ok {
			s.Log.Warn("SIPREC SRC: recording server refused a participant's stream",
				"leg_id", l.ID(), "participant", party.id)
			continue
		}
		sm, ok := l.StreamMedia(info.ID)
		if !ok || sm.Writer == nil {
			s.Log.Warn("SIPREC SRC: stream carries no writer",
				"leg_id", l.ID(), "stream_id", info.ID)
			continue
		}

		pr, pw := createPipe()
		rate := sm.SampleRate
		if rate <= 0 {
			rate = mix.SampleRate()
		}
		mix.SetParticipantRecordTap(party.mixerID, mixer.NewResampleWriter(pw, mix.SampleRate(), rate))
		s.startSRCPump(sm.Writer, pr)

		src.mu.Lock()
		src.pumps = append(src.pumps, &srcPump{
			participantID: party.mixerID,
			streamID:      info.ID,
			pipe:          pw,
		})
		src.mu.Unlock()
	}
}

// startSRCPump drains a party's audio into its recording-session stream. The
// copy ends when the pipe is closed on teardown or the leg's writer gives up
// with its context.
func (s *Server) startSRCPump(dst io.Writer, src io.Reader) {
	go func() { _, _ = io.Copy(dst, src) }()
}

func srcParticipantInfos(parties []srcParty) []siprec.ParticipantInfo {
	out := make([]siprec.ParticipantInfo, 0, len(parties))
	for _, p := range parties {
		out = append(out, siprec.ParticipantInfo{ID: p.id, AOR: p.aor})
	}
	return out
}

func srcStreamViews(l *leg.SIPLeg, parties []srcParty, labels map[string]string) []events.SIPRECStream {
	byLabel := make(map[string]leg.StreamInfo, len(parties))
	for _, info := range l.Streams() {
		byLabel[info.Label] = info
	}
	out := make([]events.SIPRECStream, 0, len(parties))
	for _, party := range parties {
		label := labels[party.id]
		st := events.SIPRECStream{
			Label:          label,
			ParticipantID:  party.id,
			ParticipantAOR: party.aor,
		}
		if info, ok := byLabel[label]; ok {
			st.LegStreamID = info.ID
		}
		out = append(out, st)
	}
	return out
}

// cleanupSIPRECSRC stops the pumps and clears the mixer taps for an outbound
// recording session. Called from cleanupLeg, so it tolerates any other leg.
func (s *Server) cleanupSIPRECSRC(l leg.Leg) {
	s.siprecMu.Lock()
	src, ok := s.siprecSRCs[l.ID()]
	delete(s.siprecSRCs, l.ID())
	s.siprecMu.Unlock()
	if !ok {
		return
	}

	var mix *mixer.Mixer
	if rm, rmOK := s.RoomMgr.Get(src.roomID); rmOK {
		mix = rm.Mixer()
	}

	src.mu.Lock()
	pumps := src.pumps
	src.pumps = nil
	src.mu.Unlock()

	for _, p := range pumps {
		switch {
		case p.legID != "":
			if src, ok := s.LegMgr.Get(p.legID); ok {
				if sl, isSIP := src.(*leg.SIPLeg); isSIP {
					sl.ClearSRCTaps()
				}
			}
		case mix != nil:
			mix.ClearParticipantRecordTap(p.participantID)
		}
		// Closing the pipe ends the pump goroutine.
		p.pipe.Close()
	}

	s.Bus.Publish(events.SIPRECSessionEnded, &events.SIPRECSessionEndedData{
		LegScope: events.LegScope{LegID: l.ID(), AppID: l.AppID()},
	})
}

// doStartLegSIPREC forks a single call to a recording server as a two-stream
// session: what the far end says, and what we send them. No room is involved,
// so the audio is tapped off the leg itself.
func (s *Server) doStartLegSIPREC(ctx context.Context, legID string, req StartSIPRECRequest) (*LegView, error) {
	if !s.Config.SIPRECSRCEnabled {
		return nil, newAPIError(http.StatusForbidden, "outbound SIPREC is disabled; set SIPREC_SRC_ENABLED=true")
	}
	recipient, err := srcRecipient(req)
	if err != nil {
		return nil, err
	}

	l, ok := s.LegMgr.Get(legID)
	if !ok {
		return nil, newAPIError(http.StatusNotFound, "leg not found")
	}
	sl, ok := l.(*leg.SIPLeg)
	if !ok {
		return nil, newAPIError(http.StatusBadRequest, "only SIP legs can be forked to a recording server")
	}
	if l.Type().IsSIPREC() {
		return nil, newAPIError(http.StatusConflict, "a recording session cannot itself be recorded")
	}

	sessionID := req.SessionID
	if sessionID == "" {
		sessionID = legID
	}
	// Two parties: the far end, whose audio we receive, and this server, whose
	// audio we send. Each gets its own m= section.
	parties := []srcParty{
		{id: legID + "#in", aor: srcParticipantAOR(l)},
		{id: legID + "#out", aor: "sip:" + legID + "+local@voiceblender.local"},
	}

	call, labels, err := s.inviteSRS(ctx, recipient, sessionID, parties, req)
	if err != nil {
		return nil, err
	}

	out := leg.NewSIPRECOutboundLeg(call, s.SIPEngine, s.Log)
	out.SetAppID(req.AppID)
	s.LegMgr.Add(out)

	src := &siprecSRC{legID: out.ID()}
	s.attachLegSRCPumps(out, sl, parties, labels, src)

	s.siprecMu.Lock()
	s.siprecSRCs[out.ID()] = src
	s.siprecMu.Unlock()

	s.Bus.Publish(events.SIPRECSessionStarted, &events.SIPRECSessionStartedData{
		LegScope:     events.LegScope{LegID: out.ID(), AppID: out.AppID()},
		SessionID:    sessionID,
		DataMode:     siprec.DataModeComplete,
		Participants: srcParticipantInfos(parties),
		Streams:      srcStreamViews(out, parties, labels),
	})

	go s.watchLegDialogEnd(out, call.Dialog.Context(), 0)

	view := s.toLegView(out)
	return &view, nil
}

// attachLegSRCPumps feeds a single call's own in and out audio into the two
// sections of its recording session.
func (s *Server) attachLegSRCPumps(out *leg.SIPLeg, source *leg.SIPLeg, parties []srcParty, labels map[string]string, src *siprecSRC) {
	byLabel := make(map[string]leg.StreamInfo, len(parties))
	for _, info := range out.Streams() {
		byLabel[info.Label] = info
	}

	writerFor := func(party srcParty) (io.Writer, string, int, bool) {
		info, ok := byLabel[labels[party.id]]
		if !ok {
			return nil, "", 0, false
		}
		sm, ok := out.StreamMedia(info.ID)
		if !ok || sm.Writer == nil {
			return nil, "", 0, false
		}
		rate := sm.SampleRate
		if rate <= 0 {
			rate = source.SampleRate()
		}
		return sm.Writer, info.ID, rate, true
	}

	var inW, outW io.Writer
	for i, party := range parties {
		dst, streamID, rate, ok := writerFor(party)
		if !ok {
			s.Log.Warn("SIPREC SRC: recording server refused a section",
				"leg_id", out.ID(), "participant", party.id)
			continue
		}
		pr, pw := createPipe()
		w := mixer.NewResampleWriter(pw, source.SampleRate(), rate)
		if i == 0 {
			inW = w
		} else {
			outW = w
		}
		s.startSRCPump(dst, pr)

		src.mu.Lock()
		src.pumps = append(src.pumps, &srcPump{
			participantID: party.id,
			streamID:      streamID,
			pipe:          pw,
			legID:         source.ID(),
		})
		src.mu.Unlock()
	}

	// Installed once: the leg holds a single pair of recording-server taps.
	source.SetSRCTaps(inW, outW)
}

// srcRecipient validates and parses the recording server URI.
func srcRecipient(req StartSIPRECRequest) (sip.Uri, error) {
	var recipient sip.Uri
	if req.SRSURI == "" {
		return recipient, newAPIError(http.StatusBadRequest, "srs_uri is required")
	}
	if err := sip.ParseUri(req.SRSURI, &recipient); err != nil || recipient.Host == "" {
		return recipient, newAPIError(http.StatusBadRequest, "invalid srs_uri %q", req.SRSURI)
	}
	return recipient, nil
}

// inviteSRS originates the recording session itself: one sendonly section per
// party, with the metadata document alongside the offer.
func (s *Server) inviteSRS(ctx context.Context, recipient sip.Uri, sessionID string, parties []srcParty, req StartSIPRECRequest) (*sipmod.OutboundCall, map[string]string, error) {
	rec, labels := buildSRCMetadata(sessionID, parties)
	md, err := rec.Marshal()
	if err != nil {
		return nil, nil, newAPIError(http.StatusInternalServerError, "build recording metadata: %s", err.Error())
	}

	streams := make([]sipmod.OfferStream, 0, len(parties))
	for _, p := range parties {
		streams = append(streams, sipmod.OfferStream{
			Direction: sipmod.DirSendOnly,
			Content:   "main",
			Label:     labels[p.id],
		})
	}

	headers := []sip.Header{sip.NewHeader("Require", sipmod.OptionTagSIPREC)}
	for k, v := range req.Headers {
		headers = append(headers, sip.NewHeader(k, v))
	}

	call, err := s.SIPEngine.Invite(ctx, recipient, sipmod.InviteOptions{
		Streams:      streams,
		Headers:      headers,
		AuthUsername: req.AuthUsername,
		AuthPassword: req.AuthPassword,
		BodyParts: []sipmod.BodyPart{{
			ContentType: sipmod.ContentTypeRSMetadata,
			Disposition: "recording-session;handling=required",
			Data:        md,
		}},
	})
	if err != nil {
		return nil, nil, newAPIError(http.StatusBadGateway, "recording server rejected the session: %s", err.Error())
	}
	return call, labels, nil
}

// --- HTTP ---

func (s *Server) startLegSIPREC(w http.ResponseWriter, r *http.Request) {
	var req StartSIPRECRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	view, err := s.doStartLegSIPREC(r.Context(), chi.URLParam(r, "id"), req)
	if err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

func (s *Server) startRoomSIPREC(w http.ResponseWriter, r *http.Request) {
	var req StartSIPRECRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	view, err := s.doStartRoomSIPREC(r.Context(), chi.URLParam(r, "id"), req)
	if err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, view)
}
