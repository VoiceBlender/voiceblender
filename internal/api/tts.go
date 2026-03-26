package api

import (
	"context"
	"io"
	"net/http"

	"github.com/csiwek/VoiceBlender/internal/events"
	"github.com/csiwek/VoiceBlender/internal/playback"
	"github.com/csiwek/VoiceBlender/internal/tts"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type ttsRequest struct {
	Text    string `json:"text"`
	Voice   string `json:"voice"`
	ModelID string `json:"model_id"`
	Volume  int    `json:"volume"`
	APIKey  string `json:"api_key,omitempty"`
}

func (s *Server) ttsLeg(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	l, ok := s.LegMgr.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "leg not found")
		return
	}

	var req ttsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	apiKey := req.APIKey
	if apiKey == "" {
		apiKey = s.Config.ElevenLabsAPIKey
	}
	if apiKey == "" {
		writeError(w, http.StatusServiceUnavailable, "no ElevenLabs API key provided")
		return
	}

	if req.Text == "" {
		writeError(w, http.StatusBadRequest, "text is required")
		return
	}
	if req.Voice == "" {
		writeError(w, http.StatusBadRequest, "voice is required")
		return
	}
	if req.Volume < -8 || req.Volume > 8 {
		writeError(w, http.StatusBadRequest, "volume must be between -8 and 8")
		return
	}

	writer := l.AudioWriter()
	if writer == nil {
		writeError(w, http.StatusConflict, "leg has no audio writer")
		return
	}

	playbackID := "pb-" + uuid.New().String()[:8]
	player := playback.NewPlayer(s.Log)
	player.SetVolume(req.Volume)

	legPlayers.Lock()
	if legPlayers.m[id] == nil {
		legPlayers.m[id] = make(map[string]*playback.Player)
	}
	legPlayers.m[id][playbackID] = player
	legPlayers.Unlock()

	go func() {
		result, err := s.TTS.Synthesize(l.Context(), req.Text, tts.Options{
			Voice:   req.Voice,
			ModelID: req.ModelID,
			APIKey:  apiKey,
		})
		if err != nil {
			legPlayers.Lock()
			delete(legPlayers.m[id], playbackID)
			if len(legPlayers.m[id]) == 0 {
				delete(legPlayers.m, id)
			}
			legPlayers.Unlock()
			s.Bus.Publish(events.PlaybackError, map[string]interface{}{"leg_id": id, "playback_id": playbackID, "error": err.Error()})
			return
		}
		defer result.Audio.Close()

		player.OnStart(func() {
			s.Bus.Publish(events.PlaybackStarted, map[string]interface{}{"leg_id": id, "playback_id": playbackID})
		})

		playErr := player.PlayReaderAtRate(l.Context(), writer, result.Audio, result.MimeType, uint32(l.SampleRate()))

		legPlayers.Lock()
		delete(legPlayers.m[id], playbackID)
		if len(legPlayers.m[id]) == 0 {
			delete(legPlayers.m, id)
		}
		legPlayers.Unlock()

		if playErr != nil && playErr != context.Canceled {
			s.Bus.Publish(events.PlaybackError, map[string]interface{}{"leg_id": id, "playback_id": playbackID, "error": playErr.Error()})
		} else {
			s.Bus.Publish(events.PlaybackFinished, map[string]interface{}{"leg_id": id, "playback_id": playbackID})
		}
	}()

	writeJSON(w, http.StatusOK, map[string]string{"playback_id": playbackID, "status": "playing"})
}

func (s *Server) ttsRoom(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	rm, ok := s.RoomMgr.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "room not found")
		return
	}

	var req ttsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	apiKey := req.APIKey
	if apiKey == "" {
		apiKey = s.Config.ElevenLabsAPIKey
	}
	if apiKey == "" {
		writeError(w, http.StatusServiceUnavailable, "no ElevenLabs API key provided")
		return
	}

	if req.Text == "" {
		writeError(w, http.StatusBadRequest, "text is required")
		return
	}
	if req.Voice == "" {
		writeError(w, http.StatusBadRequest, "voice is required")
		return
	}
	if req.Volume < -8 || req.Volume > 8 {
		writeError(w, http.StatusBadRequest, "volume must be between -8 and 8")
		return
	}

	parts := rm.Participants()
	if len(parts) == 0 {
		writeError(w, http.StatusConflict, "room has no participants")
		return
	}

	playbackID := "pb-" + uuid.New().String()[:8]

	pr, pw := io.Pipe()
	rm.Mixer().AddPlaybackSource(playbackID, pr)

	player := playback.NewPlayer(s.Log)
	player.SetVolume(req.Volume)

	roomPlayers.Lock()
	if roomPlayers.m[id] == nil {
		roomPlayers.m[id] = make(map[string]*playback.Player)
	}
	roomPlayers.m[id][playbackID] = player
	roomPlayers.Unlock()

	go func() {
		result, err := s.TTS.Synthesize(parts[0].Context(), req.Text, tts.Options{
			Voice:   req.Voice,
			ModelID: req.ModelID,
			APIKey:  apiKey,
		})
		if err != nil {
			pw.Close()
			rm.Mixer().RemoveParticipant(playbackID)
			roomPlayers.Lock()
			delete(roomPlayers.m[id], playbackID)
			if len(roomPlayers.m[id]) == 0 {
				delete(roomPlayers.m, id)
			}
			roomPlayers.Unlock()
			s.Bus.Publish(events.PlaybackError, map[string]interface{}{"room_id": id, "playback_id": playbackID, "error": err.Error()})
			return
		}
		defer result.Audio.Close()

		player.OnStart(func() {
			s.Bus.Publish(events.PlaybackStarted, map[string]interface{}{"room_id": id, "playback_id": playbackID})
		})

		playErr := player.PlayReader(parts[0].Context(), pw, result.Audio, result.MimeType)
		pw.Close()
		rm.Mixer().RemoveParticipant(playbackID)

		roomPlayers.Lock()
		delete(roomPlayers.m[id], playbackID)
		if len(roomPlayers.m[id]) == 0 {
			delete(roomPlayers.m, id)
		}
		roomPlayers.Unlock()

		if playErr != nil && playErr != context.Canceled {
			s.Log.Debug("room TTS playback error", "room_id", id, "error", playErr)
			s.Bus.Publish(events.PlaybackError, map[string]interface{}{"room_id": id, "playback_id": playbackID, "error": playErr.Error()})
		} else {
			s.Bus.Publish(events.PlaybackFinished, map[string]interface{}{"room_id": id, "playback_id": playbackID})
		}
	}()

	writeJSON(w, http.StatusOK, map[string]string{"playback_id": playbackID, "status": "playing"})
}
