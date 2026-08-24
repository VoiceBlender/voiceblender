package api

import (
	"context"
	"io"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/VoiceBlender/voiceblender/internal/mixer"
	"github.com/VoiceBlender/voiceblender/internal/stt"
	"github.com/go-chi/chi/v5"
)

// roomSTTState holds per-room STT state: active transcribers and the options
// used to start them so that new legs joining the room get STT automatically.
type roomSTTState struct {
	transcribers map[string]stt.Provider // legID -> Provider
	// opts is a template shared by every leg in the room and must be stored
	// with nil callbacks; attachSTTSinks binds them to a per-leg copy.
	opts     stt.Options
	apiKey   string
	provider string // "elevenlabs" (default), "deepgram", "deepgram_flux", "azure" or "speechmatics"
}

var (
	legTranscribers = struct {
		sync.Mutex
		m map[string]stt.Provider
	}{m: make(map[string]stt.Provider)}

	roomTranscribers = struct {
		sync.Mutex
		m map[string]*roomSTTState
	}{m: make(map[string]*roomSTTState)}
)

// newSTTProvider builds a transcriber for the named provider.
func (s *Server) newSTTProvider(provider string) stt.Provider {
	switch provider {
	case "deepgram":
		return stt.NewDeepgram(s.Log)
	case "deepgram_flux":
		return stt.NewDeepgramFlux(s.Log)
	case "azure":
		return stt.NewAzure(s.Config.AzureSpeechRegion, s.Log)
	case "speechmatics":
		return stt.NewSpeechmatics(s.Config.SpeechmaticsURL, s.Log)
	default:
		return stt.NewElevenLabs(s.Log)
	}
}

// sttAPIKey resolves the API key for the request, falling back to config, and
// returns the provider name to quote in errors.
func (s *Server) sttAPIKey(req STTRequest) (apiKey, providerName string) {
	providerName = req.Provider
	if providerName == "" {
		providerName = "elevenlabs"
	}
	if req.APIKey != "" {
		return req.APIKey, providerName
	}
	switch req.Provider {
	case "deepgram", "deepgram_flux":
		return s.Config.DeepgramAPIKey, providerName
	case "azure":
		return s.Config.AzureSpeechKey, providerName
	case "speechmatics":
		return s.Config.SpeechmaticsAPIKey, providerName
	default:
		return s.Config.ElevenLabsAPIKey, providerName
	}
}

// sttOptions maps the request onto provider options. Fields that do not apply
// to the chosen provider are ignored rather than rejected, so switching
// providers never turns a previously valid request into an error.
func sttOptions(req STTRequest) (stt.Options, error) {
	if v := req.EagerEOTThreshold; v != nil && (*v < 0.3 || *v > 0.9) {
		return stt.Options{}, newAPIError(http.StatusBadRequest, "eager_eot_threshold must be between 0.3 and 0.9")
	}
	if v := req.EOTThreshold; v != nil && (*v < 0 || *v > 1) {
		return stt.Options{}, newAPIError(http.StatusBadRequest, "eot_threshold must be between 0 and 1")
	}
	if v := req.Endpointing; v != nil && *v < 0 {
		return stt.Options{}, newAPIError(http.StatusBadRequest, "endpointing must not be negative")
	}
	if v := req.UtteranceEndMs; v != nil && *v < 0 {
		return stt.Options{}, newAPIError(http.StatusBadRequest, "utterance_end_ms must not be negative")
	}
	if v := req.EOTTimeoutMs; v != nil && *v < 0 {
		return stt.Options{}, newAPIError(http.StatusBadRequest, "eot_timeout_ms must not be negative")
	}
	return stt.Options{
		Language:          req.Language,
		Partial:           req.Partial,
		Model:             req.Model,
		Endpointing:       req.Endpointing,
		UtteranceEndMs:    req.UtteranceEndMs,
		EagerEOTThreshold: req.EagerEOTThreshold,
		EOTThreshold:      req.EOTThreshold,
		EOTTimeoutMs:      req.EOTTimeoutMs,
		LanguageHints:     req.LanguageHints,
		Keyterms:          req.Keyterms,
	}, nil
}

// attachSTTSinks returns a copy of opts with the bus callbacks bound to one
// leg. It returns a copy rather than mutating, because a room shares a single
// Options template across its legs and binding in place would route every
// leg's transcripts to whichever leg started last.
// streamID names the leg's audio stream being transcribed, empty for a leg
// whose audio is the call itself; it is what tells a recording session's
// parties apart, since they share a leg.
func (s *Server) attachSTTSinks(opts stt.Options, scope events.LegRoomScope, streamID string) stt.Options {
	bus := s.Bus
	legID := scope.LegID
	opts.OnTranscript = func(ev stt.TranscriptEvent) {
		s.Log.Info("stt callback fired", "leg_id", legID, "stream_id", streamID, "is_final", ev.IsFinal, "text_len", len(ev.Text))
		s.Log.Debug("stt callback text", "leg_id", legID, "stream_id", streamID, "is_final", ev.IsFinal, "text", ev.Text)
		bus.Publish(events.STTText, &events.STTTextData{
			LegRoomScope: scope,
			StreamID:     streamID,
			Text:         ev.Text,
			IsFinal:      ev.IsFinal,
			SpeechFinal:  ev.SpeechFinal,
			AudioStartMs: secondsToMs(ev.AudioStart),
			AudioEndMs:   secondsToMs(ev.AudioEnd),
		})
	}
	opts.OnTurn = func(ev stt.TurnEvent) {
		s.Log.Debug("stt turn event", "leg_id", legID, "stream_id", streamID, "event", ev.Event, "turn_index", ev.TurnIndex)
		bus.Publish(events.STTTurn, &events.STTTurnData{
			LegRoomScope:        scope,
			StreamID:            streamID,
			Event:               ev.Event,
			TurnIndex:           ev.TurnIndex,
			Text:                ev.Transcript,
			EndOfTurnConfidence: ev.EndOfTurnConfidence,
			AudioWindowStartMs:  secondsToMs(ev.AudioWindowStart),
			AudioWindowEndMs:    secondsToMs(ev.AudioWindowEnd),
			LastWordEndMs:       secondsToMs(ev.LastWordEnd),
			Words:               sttWords(ev.Words),
			Languages:           ev.Languages,
		})
	}
	return opts
}

func secondsToMs(sec float64) int {
	return int(math.Round(sec * 1000))
}

func sttWords(in []stt.TurnWord) []events.STTWord {
	if len(in) == 0 {
		return nil
	}
	out := make([]events.STTWord, len(in))
	for i, w := range in {
		out[i] = events.STTWord{
			Word:       w.Word,
			Confidence: w.Confidence,
			StartMs:    secondsToMs(w.Start),
			EndMs:      secondsToMs(w.End),
		}
	}
	return out
}

// STTStartLegResult is the success payload for starting STT on a leg.
type STTStartLegResult struct {
	Status string `json:"status"`
	LegID  string `json:"leg_id"`
}

func (s *Server) doStartSTTLeg(legID string, req STTRequest) (*STTStartLegResult, error) {
	apiKey, providerName := s.sttAPIKey(req)
	if apiKey == "" {
		return nil, newAPIError(http.StatusServiceUnavailable, "no %s API key provided", providerName)
	}
	opts, err := sttOptions(req)
	if err != nil {
		return nil, err
	}

	id := legID
	l, ok := s.LegMgr.Get(id)
	if !ok {
		return nil, newAPIError(http.StatusNotFound, "leg not found")
	}
	if l.State() != leg.StateConnected {
		return nil, newAPIError(http.StatusConflict, "leg not connected")
	}
	// Such a leg has no "the call" to transcribe, and AudioReader below would be
	// a second reader over the frame channel the stream's mixer participant is
	// already draining, halving both. Its room is the right scope.
	if si, ok := l.(interface{ StreamsIndependent() bool }); ok && si.StreamsIndependent() {
		return nil, newAPIError(http.StatusConflict,
			"this leg's audio is per-stream; start STT on its room instead")
	}

	legTranscribers.Lock()
	if _, exists := legTranscribers.m[id]; exists {
		legTranscribers.Unlock()
		return nil, newAPIError(http.StatusConflict, "STT already running on this leg")
	}
	transcriber := s.newSTTProvider(req.Provider)
	legTranscribers.m[id] = transcriber
	legTranscribers.Unlock()

	var reader interface{ Read([]byte) (int, error) }

	if roomID := l.RoomID(); roomID != "" {
		rm, ok := s.RoomMgr.Get(roomID)
		if !ok {
			legTranscribers.Lock()
			delete(legTranscribers.m, id)
			legTranscribers.Unlock()
			return nil, newAPIError(http.StatusConflict, "room not found")
		}
		pr, pw := createPipe()
		rm.Mixer().SetParticipantTap(id, pw)
		reader = mixer.NewResampleReader(pr, rm.Mixer().SampleRate(), mixer.DefaultSampleRate)
		_ = pw
	} else {
		ar := l.AudioReader()
		if ar == nil {
			legTranscribers.Lock()
			delete(legTranscribers.m, id)
			legTranscribers.Unlock()
			return nil, newAPIError(http.StatusConflict, "leg has no audio reader")
		}
		reader = mixer.NewResampleReader(ar, l.SampleRate(), mixer.DefaultSampleRate)
	}

	opts = s.attachSTTSinks(opts, events.LegRoomScope{LegID: id, AppID: l.AppID()}, "")
	inRoom := l.RoomID() != ""
	s.Log.Info("stt starting transcriber", "leg_id", id, "in_room", inRoom, "sample_rate", l.SampleRate(), "language", opts.Language, "partial", opts.Partial, "provider", providerName)

	go func() {
		err := transcriber.Start(l.Context(), reader, apiKey, opts, nil)
		s.Log.Info("stt transcriber exited", "leg_id", id, "error", err)
		if roomID := l.RoomID(); roomID != "" {
			if rm, ok := s.RoomMgr.Get(roomID); ok {
				rm.Mixer().ClearParticipantTap(id)
			}
		}
		legTranscribers.Lock()
		delete(legTranscribers.m, id)
		legTranscribers.Unlock()
	}()

	return &STTStartLegResult{Status: "stt_started", LegID: id}, nil
}

func (s *Server) sttLeg(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req STTRequest
	_ = decodeJSON(r, &req)
	res, err := s.doStartSTTLeg(id, req)
	if err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// STTStopResult is the success payload for stopping STT on a leg or room.
type STTStopResult struct {
	Status string `json:"status"`
}

func (s *Server) doStopSTTLeg(legID string) (*STTStopResult, error) {
	legTranscribers.Lock()
	transcriber, ok := legTranscribers.m[legID]
	if ok {
		delete(legTranscribers.m, legID)
	}
	legTranscribers.Unlock()
	if !ok {
		return nil, newAPIError(http.StatusNotFound, "no STT in progress")
	}
	transcriber.Stop()
	if l, ok := s.LegMgr.Get(legID); ok {
		if roomID := l.RoomID(); roomID != "" {
			if rm, ok := s.RoomMgr.Get(roomID); ok {
				rm.Mixer().ClearParticipantTap(legID)
			}
		}
	}
	return &STTStopResult{Status: "stt_stopped"}, nil
}

func (s *Server) stopSTTLeg(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	res, err := s.doStopSTTLeg(id)
	if err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// STTFinalizeResult is the success payload for flushing STT on a leg.
type STTFinalizeResult struct {
	Status string `json:"status"`
}

// doFinalizeSTTLeg flushes the provider's audio buffer and leaves the session
// running — unlike doStopSTTLeg it must NOT drop the transcriber from the map.
// Only deepgram and speechmatics implement the capability; the other providers
// answer 501 rather than pretending the flush happened.
func (s *Server) doFinalizeSTTLeg(ctx context.Context, legID string) (*STTFinalizeResult, error) {
	legTranscribers.Lock()
	transcriber, ok := legTranscribers.m[legID]
	legTranscribers.Unlock()
	if !ok {
		return nil, newAPIError(http.StatusNotFound, "no STT in progress")
	}
	f, ok := transcriber.(stt.Finalizer)
	if !ok {
		return nil, newAPIError(http.StatusNotImplemented, "STT provider does not support finalize")
	}
	if err := f.Finalize(ctx); err != nil {
		return nil, newAPIError(http.StatusConflict, "finalize failed: %v", err)
	}
	return &STTFinalizeResult{Status: "stt_finalized"}, nil
}

func (s *Server) finalizeSTTLeg(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	res, err := s.doFinalizeSTTLeg(r.Context(), id)
	if err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// STTStartRoomResult is the success payload for starting STT on a room.
type STTStartRoomResult struct {
	Status string   `json:"status"`
	RoomID string   `json:"room_id"`
	LegIDs []string `json:"leg_ids"`
}

func (s *Server) doStartSTTRoom(roomID string, req STTRequest) (*STTStartRoomResult, error) {
	apiKey, providerName := s.sttAPIKey(req)
	if apiKey == "" {
		return nil, newAPIError(http.StatusServiceUnavailable, "no %s API key provided", providerName)
	}
	opts, err := sttOptions(req)
	if err != nil {
		return nil, err
	}

	id := roomID
	rm, ok := s.RoomMgr.Get(id)
	if !ok {
		return nil, newAPIError(http.StatusNotFound, "room not found")
	}
	// Both kinds of source count: a recording session's room holds one stream
	// participant per party and no leg participant at all.
	type sttSource struct {
		participantID string
		streamID      string
		l             leg.Leg
	}
	var sources []sttSource
	for _, l := range rm.Participants() {
		sources = append(sources, sttSource{participantID: l.ID(), l: l})
	}
	for _, sp := range rm.StreamParticipants() {
		l, ok := s.LegMgr.Get(sp.LegID)
		if !ok {
			// The stream outlived its leg; nothing to hang a context on.
			s.Log.Warn("room stt: stream participant has no leg", "room_id", id,
				"participant_id", sp.ParticipantID, "leg_id", sp.LegID)
			continue
		}
		sources = append(sources, sttSource{participantID: sp.ParticipantID, streamID: sp.StreamID, l: l})
	}
	if len(sources) == 0 {
		return nil, newAPIError(http.StatusConflict, "room has no participants")
	}

	roomTranscribers.Lock()
	if _, exists := roomTranscribers.m[id]; exists {
		roomTranscribers.Unlock()
		return nil, newAPIError(http.StatusConflict, "STT already running on this room")
	}
	state := &roomSTTState{
		transcribers: make(map[string]stt.Provider),
		opts:         opts,
		apiKey:       apiKey,
		provider:     req.Provider,
	}
	roomTranscribers.m[id] = state
	roomTranscribers.Unlock()

	legIDs := make([]string, 0, len(sources))
	for _, src := range sources {
		legIDs = append(legIDs, src.participantID)
		s.startRoomLegSTT(id, src.participantID, src.streamID, src.l, rm.Mixer(), state)
	}
	return &STTStartRoomResult{Status: "stt_started", RoomID: id, LegIDs: legIDs}, nil
}

func (s *Server) sttRoom(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req STTRequest
	_ = decodeJSON(r, &req)
	res, err := s.doStartSTTRoom(id, req)
	if err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

const sttFeedInterval = 5 * time.Second

// watchSTTFeed reports what is happening between the mixer and a transcriber.
// Neither a participant that stops producing nor a transcriber that stops
// consuming raises an error anywhere — the tap must not block the media clock,
// so a full buffer is a silent discard — and both end with a party missing from
// the transcript.
func (s *Server) watchSTTFeed(ctx context.Context, pw *pipeWriter, roomID, participantID, legID, streamID string) {
	t := time.NewTicker(sttFeedInterval)
	defer t.Stop()

	// Offered alone cannot tell a producing source from a starved one: the
	// silence the mix substitutes reaches the tap like real audio.
	feed := func() (read, starved, short uint64) {
		if rm, ok := s.RoomMgr.Get(roomID); ok {
			r, st, sh, _ := rm.Mixer().ParticipantFeed(participantID)
			return r, st, sh
		}
		return 0, 0, 0
	}

	var lastOffered, lastDropped, lastRead, lastStarved, lastShort uint64
	for {
		select {
		case <-ctx.Done():
			offered, dropped := pw.Stats()
			read, starved, short := feed()
			s.Log.Info("stt feed closed", "room_id", roomID, "leg_id", legID,
				"stream_id", streamID, "offered", offered, "dropped", dropped,
				"frames_read", read, "starved", starved, "short_reads", short)
			return
		case <-t.C:
			offered, dropped := pw.Stats()
			read, starved, short := feed()
			newOffered, newDropped := offered-lastOffered, dropped-lastDropped
			newRead, newStarved, newShort := read-lastRead, starved-lastStarved, short-lastShort
			lastOffered, lastDropped, lastRead, lastStarved, lastShort = offered, dropped, read, starved, short

			switch {
			case newDropped > 0:
				s.Log.Warn("stt feed: transcriber is not keeping up, audio discarded",
					"room_id", roomID, "leg_id", legID, "stream_id", streamID,
					"offered", newOffered, "dropped", newDropped,
					"total_offered", offered, "total_dropped", dropped)
			case newRead == 0 && newOffered > 0:
				s.Log.Warn("stt feed: the source produced nothing, transcriber is being fed silence",
					"room_id", roomID, "leg_id", legID, "stream_id", streamID,
					"offered", newOffered, "starved", newStarved)
			case newOffered == 0:
				s.Log.Warn("stt feed: no audio offered to the transcriber",
					"room_id", roomID, "leg_id", legID, "stream_id", streamID,
					"interval", sttFeedInterval, "total_offered", offered)
			default:
				// Debug: a healthy feed logs this every interval per source.
				s.Log.Debug("stt feed", "room_id", roomID, "leg_id", legID,
					"stream_id", streamID, "offered", newOffered, "frames_read", newRead,
					"starved", newStarved, "short_reads", newShort)
			}
		}
	}
}

// startRoomLegSTT spins up a transcriber for one audio source within a room STT
// session. participantID is the mixer's key — a leg ID, or "legID#streamID" for
// one of a leg's streams — so a leg contributing several streams gets one
// transcriber each. streamID is empty for an ordinary participant.
// Caller must ensure state is in roomTranscribers.m[roomID].
func (s *Server) startRoomLegSTT(roomID, participantID, streamID string, l leg.Leg, mix *mixer.Mixer, state *roomSTTState) {
	pr, pw := createPipe()
	// A tap copies the participant's frames; reading the stream's io.Reader
	// directly would take them away from whoever else is draining it.
	mix.SetParticipantTap(participantID, pw)
	sttReader := io.Reader(mixer.NewResampleReader(pr, mix.SampleRate(), mixer.DefaultSampleRate))

	transcriber := s.newSTTProvider(state.provider)
	roomTranscribers.Lock()
	state.transcribers[participantID] = transcriber
	roomTranscribers.Unlock()

	apiKey := state.apiKey
	opts := s.attachSTTSinks(state.opts, events.LegRoomScope{LegID: l.ID(), RoomID: roomID, AppID: l.AppID()}, streamID)

	go s.watchSTTFeed(l.Context(), pw, roomID, participantID, l.ID(), streamID)

	go func() {
		_ = transcriber.Start(l.Context(), sttReader, apiKey, opts, nil)
		// Cleanup on exit.
		if rm, ok := s.RoomMgr.Get(roomID); ok {
			rm.Mixer().ClearParticipantTap(participantID)
		}
		roomTranscribers.Lock()
		if st, ok := roomTranscribers.m[roomID]; ok {
			delete(st.transcribers, participantID)
			if len(st.transcribers) == 0 {
				delete(roomTranscribers.m, roomID)
			}
		}
		roomTranscribers.Unlock()
	}()
}

// onLegJoinedRoom starts STT for a newly added leg if room STT is active.
func (s *Server) onLegJoinedRoom(roomID, legID string) {
	// Auto-start per-participant recording if multi-channel is active.
	s.onLegJoinedRoomRecording(roomID, legID)

	roomTranscribers.Lock()
	state, ok := roomTranscribers.m[roomID]
	if !ok {
		roomTranscribers.Unlock()
		return
	}
	if _, exists := state.transcribers[legID]; exists {
		roomTranscribers.Unlock()
		return
	}
	roomTranscribers.Unlock()

	l, ok := s.LegMgr.Get(legID)
	if !ok {
		return
	}
	rm, ok := s.RoomMgr.Get(roomID)
	if !ok {
		return
	}

	s.Log.Info("stt auto-starting for new leg in room", "room_id", roomID, "leg_id", legID)
	s.startRoomLegSTT(roomID, legID, "", l, rm.Mixer(), state)
}

func (s *Server) doStopSTTRoom(roomID string) (*STTStopResult, error) {
	roomTranscribers.Lock()
	state, ok := roomTranscribers.m[roomID]
	if ok {
		delete(roomTranscribers.m, roomID)
	}
	roomTranscribers.Unlock()
	if !ok {
		return nil, newAPIError(http.StatusNotFound, "no STT in progress")
	}
	rm, rmOK := s.RoomMgr.Get(roomID)
	for legID, transcriber := range state.transcribers {
		transcriber.Stop()
		if rmOK {
			rm.Mixer().ClearParticipantTap(legID)
		}
	}
	return &STTStopResult{Status: "stt_stopped"}, nil
}

func (s *Server) stopSTTRoom(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	res, err := s.doStopSTTRoom(id)
	if err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
