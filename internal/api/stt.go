package api

import (
	"net/http"
	"sync"

	"github.com/csiwek/VoiceBlender/internal/events"
	"github.com/csiwek/VoiceBlender/internal/leg"
	"github.com/csiwek/VoiceBlender/internal/mixer"
	"github.com/csiwek/VoiceBlender/internal/stt"
	"github.com/go-chi/chi/v5"
)

// roomSTTState holds per-room STT state: active transcribers and the options
// used to start them so that new legs joining the room get STT automatically.
type roomSTTState struct {
	transcribers map[string]*stt.Transcriber // legID -> Transcriber
	opts         stt.Options
	apiKey       string
}

var (
	legTranscribers = struct {
		sync.Mutex
		m map[string]*stt.Transcriber
	}{m: make(map[string]*stt.Transcriber)}

	roomTranscribers = struct {
		sync.Mutex
		m map[string]*roomSTTState
	}{m: make(map[string]*roomSTTState)}
)

type sttRequest struct {
	Language string `json:"language"`
	Partial  bool   `json:"partial"`
	APIKey   string `json:"api_key,omitempty"`
}

func (s *Server) sttLeg(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req sttRequest
	// Body is optional; ignore decode errors for empty body.
	_ = decodeJSON(r, &req)

	apiKey := req.APIKey
	if apiKey == "" {
		apiKey = s.Config.ElevenLabsAPIKey
	}
	if apiKey == "" {
		writeError(w, http.StatusServiceUnavailable, "no ElevenLabs API key provided")
		return
	}

	l, ok := s.LegMgr.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "leg not found")
		return
	}
	if l.State() != leg.StateConnected {
		writeError(w, http.StatusConflict, "leg not connected")
		return
	}

	legTranscribers.Lock()
	if _, exists := legTranscribers.m[id]; exists {
		legTranscribers.Unlock()
		writeError(w, http.StatusConflict, "STT already running on this leg")
		return
	}

	transcriber := stt.New(s.Log)
	legTranscribers.m[id] = transcriber
	legTranscribers.Unlock()

	var reader interface{ Read([]byte) (int, error) }
	var tapPW *pipeWriter // non-nil if we set a participant tap

	if roomID := l.RoomID(); roomID != "" {
		// Leg is in a room — use a per-participant tap from the mixer.
		rm, ok := s.RoomMgr.Get(roomID)
		if !ok {
			legTranscribers.Lock()
			delete(legTranscribers.m, id)
			legTranscribers.Unlock()
			writeError(w, http.StatusConflict, "room not found")
			return
		}
		pr, pw := createPipe()
		rm.Mixer().SetParticipantTap(id, pw)
		reader = pr
		tapPW = pw
		_ = tapPW // used in cleanup
	} else {
		// Standalone leg — read audio directly.
		ar := l.AudioReader()
		if ar == nil {
			legTranscribers.Lock()
			delete(legTranscribers.m, id)
			legTranscribers.Unlock()
			writeError(w, http.StatusConflict, "leg has no audio reader")
			return
		}
		// Resample to 16kHz if needed.
		reader = mixer.NewResampleReader(ar, l.SampleRate(), mixer.SampleRate)
	}

	bus := s.Bus
	cb := func(text string, isFinal bool) {
		s.Log.Info("stt callback fired", "leg_id", id, "text", text, "is_final", isFinal)
		data := map[string]interface{}{
			"leg_id":   id,
			"text":     text,
			"is_final": isFinal,
		}
		bus.Publish(events.STTText, data)
	}

	opts := stt.Options{Language: req.Language, Partial: req.Partial}
	inRoom := l.RoomID() != ""
	s.Log.Info("stt starting transcriber", "leg_id", id, "in_room", inRoom, "sample_rate", l.SampleRate(), "language", opts.Language, "partial", opts.Partial)

	go func() {
		err := transcriber.Start(l.Context(), reader, apiKey, opts, cb)
		s.Log.Info("stt transcriber exited", "leg_id", id, "error", err)
		// Cleanup on exit.
		if roomID := l.RoomID(); roomID != "" {
			if rm, ok := s.RoomMgr.Get(roomID); ok {
				rm.Mixer().ClearParticipantTap(id)
			}
		}
		legTranscribers.Lock()
		delete(legTranscribers.m, id)
		legTranscribers.Unlock()
	}()

	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "stt_started", "leg_id": id})
}

func (s *Server) stopSTTLeg(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	legTranscribers.Lock()
	transcriber, ok := legTranscribers.m[id]
	if ok {
		delete(legTranscribers.m, id)
	}
	legTranscribers.Unlock()

	if !ok {
		writeError(w, http.StatusNotFound, "no STT in progress")
		return
	}

	transcriber.Stop()

	// Clear participant tap if leg is in a room.
	if l, ok := s.LegMgr.Get(id); ok {
		if roomID := l.RoomID(); roomID != "" {
			if rm, ok := s.RoomMgr.Get(roomID); ok {
				rm.Mixer().ClearParticipantTap(id)
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "stt_stopped"})
}

func (s *Server) sttRoom(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req sttRequest
	_ = decodeJSON(r, &req)

	apiKey := req.APIKey
	if apiKey == "" {
		apiKey = s.Config.ElevenLabsAPIKey
	}
	if apiKey == "" {
		writeError(w, http.StatusServiceUnavailable, "no ElevenLabs API key provided")
		return
	}

	rm, ok := s.RoomMgr.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "room not found")
		return
	}

	parts := rm.Participants()
	if len(parts) == 0 {
		writeError(w, http.StatusConflict, "room has no participants")
		return
	}

	roomTranscribers.Lock()
	if _, exists := roomTranscribers.m[id]; exists {
		roomTranscribers.Unlock()
		writeError(w, http.StatusConflict, "STT already running on this room")
		return
	}
	opts := stt.Options{Language: req.Language, Partial: req.Partial}
	state := &roomSTTState{
		transcribers: make(map[string]*stt.Transcriber),
		opts:         opts,
		apiKey:       apiKey,
	}
	roomTranscribers.m[id] = state
	roomTranscribers.Unlock()

	roomID := id
	legIDs := make([]string, 0, len(parts))

	for _, l := range parts {
		legID := l.ID()
		legIDs = append(legIDs, legID)
		s.startRoomLegSTT(roomID, legID, l, rm.Mixer(), state)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":  "stt_started",
		"room_id": id,
		"leg_ids": legIDs,
	})
}

// startRoomLegSTT spins up a transcriber for a single leg within a room STT session.
// Caller must ensure state is in roomTranscribers.m[roomID].
func (s *Server) startRoomLegSTT(roomID, legID string, l leg.Leg, mix *mixer.Mixer, state *roomSTTState) {
	pr, pw := createPipe()
	mix.SetParticipantTap(legID, pw)

	transcriber := stt.New(s.Log)
	roomTranscribers.Lock()
	state.transcribers[legID] = transcriber
	roomTranscribers.Unlock()

	bus := s.Bus
	opts := state.opts
	apiKey := state.apiKey

	cb := func(text string, isFinal bool) {
		data := map[string]interface{}{
			"leg_id":   legID,
			"room_id":  roomID,
			"text":     text,
			"is_final": isFinal,
		}
		bus.Publish(events.STTText, data)
	}

	go func() {
		_ = transcriber.Start(l.Context(), pr, apiKey, opts, cb)
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

func (s *Server) stopSTTRoom(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	roomTranscribers.Lock()
	state, ok := roomTranscribers.m[id]
	if ok {
		delete(roomTranscribers.m, id)
	}
	roomTranscribers.Unlock()

	if !ok {
		writeError(w, http.StatusNotFound, "no STT in progress")
		return
	}

	rm, rmOK := s.RoomMgr.Get(id)
	for legID, transcriber := range state.transcribers {
		transcriber.Stop()
		if rmOK {
			rm.Mixer().ClearParticipantTap(legID)
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "stt_stopped"})
}
