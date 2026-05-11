package api

import (
	"io"
	"net/http"
	"sync"

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
	opts         stt.Options
	apiKey       string
	provider     string // "elevenlabs" (default) or "deepgram"
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

// STTStartLegResult is the success payload for starting STT on a leg.
type STTStartLegResult struct {
	Status string `json:"status"`
	LegID  string `json:"leg_id"`
}

func (s *Server) doStartSTTLeg(legID string, req STTRequest) (*STTStartLegResult, error) {
	apiKey := req.APIKey
	if apiKey == "" {
		switch req.Provider {
		case "deepgram":
			apiKey = s.Config.DeepgramAPIKey
		case "azure":
			apiKey = s.Config.AzureSpeechKey
		default:
			apiKey = s.Config.ElevenLabsAPIKey
		}
	}
	if apiKey == "" {
		providerName := req.Provider
		if providerName == "" {
			providerName = "elevenlabs"
		}
		return nil, newAPIError(http.StatusServiceUnavailable, "no %s API key provided", providerName)
	}

	id := legID
	l, ok := s.LegMgr.Get(id)
	if !ok {
		return nil, newAPIError(http.StatusNotFound, "leg not found")
	}
	if l.State() != leg.StateConnected {
		return nil, newAPIError(http.StatusConflict, "leg not connected")
	}

	legTranscribers.Lock()
	if _, exists := legTranscribers.m[id]; exists {
		legTranscribers.Unlock()
		return nil, newAPIError(http.StatusConflict, "STT already running on this leg")
	}
	var transcriber stt.Provider
	switch req.Provider {
	case "deepgram":
		transcriber = stt.NewDeepgram(s.Log)
	case "azure":
		transcriber = stt.NewAzure(s.Config.AzureSpeechRegion, s.Log)
	default:
		transcriber = stt.NewElevenLabs(s.Log)
	}
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

	bus := s.Bus
	legAppID := l.AppID()
	cb := func(text string, isFinal bool) {
		s.Log.Info("stt callback fired", "leg_id", id, "text", text, "is_final", isFinal)
		bus.Publish(events.STTText, &events.STTTextData{
			LegRoomScope: events.LegRoomScope{LegID: id, AppID: legAppID},
			Text:         text,
			IsFinal:      isFinal,
		})
	}

	opts := stt.Options{Language: req.Language, Partial: req.Partial}
	inRoom := l.RoomID() != ""
	s.Log.Info("stt starting transcriber", "leg_id", id, "in_room", inRoom, "sample_rate", l.SampleRate(), "language", opts.Language, "partial", opts.Partial)

	go func() {
		err := transcriber.Start(l.Context(), reader, apiKey, opts, cb)
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

// STTStartRoomResult is the success payload for starting STT on a room.
type STTStartRoomResult struct {
	Status string   `json:"status"`
	RoomID string   `json:"room_id"`
	LegIDs []string `json:"leg_ids"`
}

func (s *Server) doStartSTTRoom(roomID string, req STTRequest) (*STTStartRoomResult, error) {
	apiKey := req.APIKey
	if apiKey == "" {
		switch req.Provider {
		case "deepgram":
			apiKey = s.Config.DeepgramAPIKey
		case "azure":
			apiKey = s.Config.AzureSpeechKey
		default:
			apiKey = s.Config.ElevenLabsAPIKey
		}
	}
	if apiKey == "" {
		providerName := req.Provider
		if providerName == "" {
			providerName = "elevenlabs"
		}
		return nil, newAPIError(http.StatusServiceUnavailable, "no %s API key provided", providerName)
	}

	id := roomID
	rm, ok := s.RoomMgr.Get(id)
	if !ok {
		return nil, newAPIError(http.StatusNotFound, "room not found")
	}
	parts := rm.Participants()
	if len(parts) == 0 {
		return nil, newAPIError(http.StatusConflict, "room has no participants")
	}

	roomTranscribers.Lock()
	if _, exists := roomTranscribers.m[id]; exists {
		roomTranscribers.Unlock()
		return nil, newAPIError(http.StatusConflict, "STT already running on this room")
	}
	opts := stt.Options{Language: req.Language, Partial: req.Partial}
	state := &roomSTTState{
		transcribers: make(map[string]stt.Provider),
		opts:         opts,
		apiKey:       apiKey,
		provider:     req.Provider,
	}
	roomTranscribers.m[id] = state
	roomTranscribers.Unlock()

	legIDs := make([]string, 0, len(parts))
	for _, l := range parts {
		legID := l.ID()
		legIDs = append(legIDs, legID)
		s.startRoomLegSTT(id, legID, l, rm.Mixer(), state)
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

// startRoomLegSTT spins up a transcriber for a single leg within a room STT session.
// Caller must ensure state is in roomTranscribers.m[roomID].
func (s *Server) startRoomLegSTT(roomID, legID string, l leg.Leg, mix *mixer.Mixer, state *roomSTTState) {
	pr, pw := createPipe()
	mix.SetParticipantTap(legID, pw)
	sttReader := io.Reader(mixer.NewResampleReader(pr, mix.SampleRate(), mixer.DefaultSampleRate))

	var transcriber stt.Provider
	switch state.provider {
	case "deepgram":
		transcriber = stt.NewDeepgram(s.Log)
	case "azure":
		transcriber = stt.NewAzure(s.Config.AzureSpeechRegion, s.Log)
	default:
		transcriber = stt.NewElevenLabs(s.Log)
	}
	roomTranscribers.Lock()
	state.transcribers[legID] = transcriber
	roomTranscribers.Unlock()

	bus := s.Bus
	opts := state.opts
	apiKey := state.apiKey
	sttAppID := l.AppID()

	cb := func(text string, isFinal bool) {
		bus.Publish(events.STTText, &events.STTTextData{
			LegRoomScope: events.LegRoomScope{LegID: legID, RoomID: roomID, AppID: sttAppID},
			Text:         text,
			IsFinal:      isFinal,
		})
	}

	go func() {
		_ = transcriber.Start(l.Context(), sttReader, apiKey, opts, cb)
		// Cleanup on exit.
		if rm, ok := s.RoomMgr.Get(roomID); ok {
			rm.Mixer().ClearParticipantTap(legID)
		}
		roomTranscribers.Lock()
		if st, ok := roomTranscribers.m[roomID]; ok {
			delete(st.transcribers, legID)
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
	s.startRoomLegSTT(roomID, legID, l, rm.Mixer(), state)
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
