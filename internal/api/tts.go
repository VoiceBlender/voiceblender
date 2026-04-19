package api

import (
	"context"
	"io"
	"net/http"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/mixer"
	"github.com/VoiceBlender/voiceblender/internal/playback"
	"github.com/VoiceBlender/voiceblender/internal/tts"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func (s *Server) ttsLeg(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	l, ok := s.LegMgr.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "leg not found")
		return
	}

	var req TTSRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	provider, apiKey := s.resolveTTSProvider(req)
	if provider == nil {
		providerName := req.Provider
		if providerName == "" {
			providerName = "elevenlabs"
		}
		writeError(w, http.StatusServiceUnavailable, "no "+providerName+" API key provided")
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

	directWriter := l.AudioWriter()
	if directWriter == nil {
		writeError(w, http.StatusConflict, "leg has no audio writer")
		return
	}

	// Route through the mixer inject channel when the leg is in a room,
	// identical to playLeg. This prevents contention on the leg's outFrames
	// channel which the mixer writeLoop already owns.
	writer := &legPlaybackWriter{
		legID:        id,
		leg:          l,
		directWriter: directWriter,
		roomMgr:      s.RoomMgr,
	}

	ttsID := "tts-" + uuid.New().String()[:8]
	appID := l.AppID()
	player := playback.NewPlayer(s.Log)
	player.SetVolume(req.Volume)

	legPlayers.Lock()
	if legPlayers.m[id] == nil {
		legPlayers.m[id] = make(map[string]*playback.Player)
	}
	legPlayers.m[id][ttsID] = player
	legPlayers.Unlock()

	go func() {
		result, err := provider.Synthesize(l.Context(), req.Text, tts.Options{
			Voice:    req.Voice,
			ModelID:  req.ModelID,
			Language: req.Language,
			Prompt:   req.Prompt,
			APIKey:   apiKey,
		})
		if err != nil {
			legPlayers.Lock()
			delete(legPlayers.m[id], ttsID)
			if len(legPlayers.m[id]) == 0 {
				delete(legPlayers.m, id)
			}
			legPlayers.Unlock()
			s.Bus.Publish(events.TTSError, &events.TTSErrorData{
				LegRoomScope: events.LegRoomScope{LegID: id, AppID: appID},
				TTSID:        ttsID,
				Error:        err.Error(),
			})
			return
		}
		defer result.Audio.Close()

		player.OnStart(func() {
			s.Bus.Publish(events.TTSStarted, &events.TTSStartedData{
				LegRoomScope: events.LegRoomScope{LegID: id, AppID: appID},
				TTSID:        ttsID,
			})
		})

		ttsRate := uint32(mixer.DefaultSampleRate)
		if roomID := l.RoomID(); roomID != "" {
			if rm, ok := s.RoomMgr.Get(roomID); ok {
				ttsRate = uint32(rm.Mixer().SampleRate())
			}
		}
		playErr := player.PlayReaderAtRate(l.Context(), writer, result.Audio, result.MimeType, ttsRate)

		legPlayers.Lock()
		delete(legPlayers.m[id], ttsID)
		if len(legPlayers.m[id]) == 0 {
			delete(legPlayers.m, id)
		}
		legPlayers.Unlock()

		if playErr != nil && playErr != context.Canceled {
			s.Bus.Publish(events.TTSError, &events.TTSErrorData{
				LegRoomScope: events.LegRoomScope{LegID: id, AppID: appID},
				TTSID:        ttsID,
				Error:        playErr.Error(),
			})
		} else {
			s.Bus.Publish(events.TTSFinished, &events.TTSFinishedData{
				LegRoomScope: events.LegRoomScope{LegID: id, AppID: appID},
				TTSID:        ttsID,
			})
		}
	}()

	writeJSON(w, http.StatusOK, map[string]string{"tts_id": ttsID, "status": "playing"})
}

func (s *Server) ttsRoom(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	rm, ok := s.RoomMgr.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "room not found")
		return
	}

	var req TTSRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	provider, apiKey := s.resolveTTSProvider(req)
	if provider == nil {
		providerName := req.Provider
		if providerName == "" {
			providerName = "elevenlabs"
		}
		writeError(w, http.StatusServiceUnavailable, "no "+providerName+" API key provided")
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

	ttsID := "tts-" + uuid.New().String()[:8]
	roomAppID := rm.AppID

	pr, pw := io.Pipe()
	rm.Mixer().AddPlaybackSource(ttsID, pr)

	player := playback.NewPlayer(s.Log)
	player.SetVolume(req.Volume)

	roomPlayers.Lock()
	if roomPlayers.m[id] == nil {
		roomPlayers.m[id] = make(map[string]*playback.Player)
	}
	roomPlayers.m[id][ttsID] = player
	roomPlayers.Unlock()

	go func() {
		result, err := provider.Synthesize(parts[0].Context(), req.Text, tts.Options{
			Voice:   req.Voice,
			ModelID: req.ModelID,
			APIKey:  apiKey,
		})
		if err != nil {
			pw.Close()
			rm.Mixer().RemoveParticipant(ttsID)
			roomPlayers.Lock()
			delete(roomPlayers.m[id], ttsID)
			if len(roomPlayers.m[id]) == 0 {
				delete(roomPlayers.m, id)
			}
			roomPlayers.Unlock()
			s.Bus.Publish(events.TTSError, &events.TTSErrorData{
				LegRoomScope: events.LegRoomScope{RoomID: id, AppID: roomAppID},
				TTSID:        ttsID,
				Error:        err.Error(),
			})
			return
		}
		defer result.Audio.Close()

		player.OnStart(func() {
			s.Bus.Publish(events.TTSStarted, &events.TTSStartedData{
				LegRoomScope: events.LegRoomScope{RoomID: id, AppID: roomAppID},
				TTSID:        ttsID,
			})
		})

		playErr := player.PlayReaderAtRate(parts[0].Context(), pw, result.Audio, result.MimeType, uint32(rm.Mixer().SampleRate()))
		pw.Close()
		rm.Mixer().RemoveParticipant(ttsID)

		roomPlayers.Lock()
		delete(roomPlayers.m[id], ttsID)
		if len(roomPlayers.m[id]) == 0 {
			delete(roomPlayers.m, id)
		}
		roomPlayers.Unlock()

		if playErr != nil && playErr != context.Canceled {
			s.Log.Debug("room TTS playback error", "room_id", id, "error", playErr)
			s.Bus.Publish(events.TTSError, &events.TTSErrorData{
				LegRoomScope: events.LegRoomScope{RoomID: id, AppID: roomAppID},
				TTSID:        ttsID,
				Error:        playErr.Error(),
			})
		} else {
			s.Bus.Publish(events.TTSFinished, &events.TTSFinishedData{
				LegRoomScope: events.LegRoomScope{RoomID: id, AppID: roomAppID},
				TTSID:        ttsID,
			})
		}
	}()

	writeJSON(w, http.StatusOK, map[string]string{"tts_id": ttsID, "status": "playing"})
}

// resolveTTSProvider returns the TTS provider and API key for the request.
// Returns nil provider if the required API key is missing.
// When a TTS cache is configured, the provider is wrapped to serve cached results.
func (s *Server) resolveTTSProvider(req TTSRequest) (tts.Provider, string) {
	apiKey := req.APIKey
	var provider tts.Provider
	var name string
	switch req.Provider {
	case "aws":
		// AWS Polly uses the default credential chain; api_key is optional
		// (format: "ACCESS_KEY:SECRET_KEY" for per-request overrides).
		provider, name = tts.NewAWS(s.Config.S3Region, s.Log), "aws"
	case "google":
		// Google Cloud TTS uses Application Default Credentials; api_key is optional.
		provider, name = tts.NewGoogle(s.Log), "google"
	case "deepgram":
		if apiKey == "" {
			apiKey = s.Config.DeepgramAPIKey
		}
		if apiKey == "" {
			return nil, ""
		}
		provider, name = tts.NewDeepgram(apiKey, s.Log), "deepgram"
	case "azure":
		if apiKey == "" {
			apiKey = s.Config.AzureSpeechKey
		}
		if apiKey == "" {
			return nil, ""
		}
		provider, name = tts.NewAzure(apiKey, s.Config.AzureSpeechRegion, s.Log), "azure"
	default:
		// ElevenLabs (default).
		if apiKey == "" {
			apiKey = s.Config.ElevenLabsAPIKey
		}
		if apiKey == "" {
			return nil, ""
		}
		provider, name = s.TTS, "elevenlabs"
	}
	if s.TTSCache != nil {
		provider = s.TTSCache.WrapProvider(provider, name)
	}
	return provider, apiKey
}
