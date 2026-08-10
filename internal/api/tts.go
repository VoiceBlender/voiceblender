package api

import (
	"context"
	"io"
	"net/http"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/VoiceBlender/voiceblender/internal/mixer"
	"github.com/VoiceBlender/voiceblender/internal/playback"
	"github.com/VoiceBlender/voiceblender/internal/tts"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// TTSStartResult is the success payload for synthesizing TTS on a leg or room.
type TTSStartResult struct {
	TTSID  string `json:"tts_id"`
	Status string `json:"status"`
}

// legTTSTarget is a validated leg TTS request: the leg, its resolved provider
// and the writer that will receive the audio.
type legTTSTarget struct {
	leg          leg.Leg
	provider     tts.Provider
	apiKey       string
	directWriter io.Writer
}

func (s *Server) validateLegTTS(legID string, req TTSRequest) (*legTTSTarget, error) {
	l, ok := s.LegMgr.Get(legID)
	if !ok {
		return nil, newAPIError(http.StatusNotFound, "leg not found")
	}
	provider, apiKey := s.resolveTTSProvider(req)
	if provider == nil {
		providerName := req.Provider
		if providerName == "" {
			providerName = "elevenlabs"
		}
		return nil, newAPIError(http.StatusServiceUnavailable, "no %s API key provided", providerName)
	}
	if req.Text == "" {
		return nil, newAPIError(http.StatusBadRequest, "text is required")
	}
	if req.Voice == "" {
		return nil, newAPIError(http.StatusBadRequest, "voice is required")
	}
	if req.Volume < -8 || req.Volume > 8 {
		return nil, newAPIError(http.StatusBadRequest, "volume must be between -8 and 8")
	}
	directWriter := l.AudioWriter()
	if directWriter == nil {
		return nil, newAPIError(http.StatusConflict, "leg has no audio writer")
	}
	return &legTTSTarget{leg: l, provider: provider, apiKey: apiKey, directWriter: directWriter}, nil
}

// registerLegTTSPlayer allocates a TTS id and registers its player, so that
// leg_play_stop can reach the utterance from the moment the command returns.
func (s *Server) registerLegTTSPlayer(legID string, volume int) (string, *playback.Player) {
	ttsID := newTTSID()
	return ttsID, s.registerLegPlayerAs(legID, ttsID, volume)
}

func (s *Server) registerLegPlayerAs(legID, playerID string, volume int) *playback.Player {
	player := playback.NewPlayer(s.Log)
	player.SetVolume(volume)

	legPlayers.Lock()
	if legPlayers.m[legID] == nil {
		legPlayers.m[legID] = make(map[string]*playback.Player)
	}
	legPlayers.m[legID][playerID] = player
	legPlayers.Unlock()

	return player
}

func newTTSID() string {
	return "tts-" + uuid.New().String()[:8]
}

func legPlayerExists(legID, playerID string) bool {
	legPlayers.Lock()
	defer legPlayers.Unlock()
	_, ok := legPlayers.m[legID][playerID]
	return ok
}

func deregisterLegPlayer(legID, playerID string) {
	legPlayers.Lock()
	delete(legPlayers.m[legID], playerID)
	if len(legPlayers.m[legID]) == 0 {
		delete(legPlayers.m, legID)
	}
	legPlayers.Unlock()
}

// playLegTTSAudio plays synthesized audio on a leg and publishes the
// tts.started / tts.finished / tts.error lifecycle. Shared by immediate and
// committed-preflight playback.
func (s *Server) playLegTTSAudio(t *legTTSTarget, ttsID string, player *playback.Player, audio io.Reader, mimeType string) {
	l := t.leg
	legID := l.ID()
	scope := events.LegRoomScope{LegID: legID, AppID: l.AppID()}

	player.OnStart(func() {
		s.Bus.Publish(events.TTSStarted, &events.TTSStartedData{LegRoomScope: scope, TTSID: ttsID})
	})

	ttsRate := uint32(mixer.DefaultSampleRate)
	if roomID := l.RoomID(); roomID != "" {
		if rm, ok := s.RoomMgr.Get(roomID); ok {
			ttsRate = uint32(rm.Mixer().SampleRate())
		}
	}
	// Built here, not at command time, so srcRate reflects whichever rate the
	// player will actually produce — the rate is decided after synthesis, and
	// the leg may have moved rooms in between.
	writer := &legPlaybackWriter{
		legID:        legID,
		leg:          l,
		directWriter: t.directWriter,
		roomMgr:      s.RoomMgr,
		srcRate:      ttsRate,
	}
	playErr := player.PlayReaderAtRate(l.Context(), writer, audio, mimeType, ttsRate)

	deregisterLegPlayer(legID, ttsID)

	if playErr != nil && playErr != context.Canceled {
		s.Bus.Publish(events.TTSError, &events.TTSErrorData{
			LegRoomScope: scope,
			TTSID:        ttsID,
			Error:        playErr.Error(),
			Category:     string(tts.CategoryPlayback),
		})
		return
	}
	s.Bus.Publish(events.TTSFinished, &events.TTSFinishedData{
		LegRoomScope: scope,
		TTSID:        ttsID,
		Reason:       playbackReason(playErr),
		PlayedMs:     player.PlayedMillis(),
	})
}

func (s *Server) doLegTTS(legID string, req TTSRequest) (*TTSStartResult, error) {
	t, err := s.validateLegTTS(legID, req)
	if err != nil {
		return nil, err
	}

	ttsID, player := s.registerLegTTSPlayer(legID, req.Volume)

	go func() {
		result, err := t.provider.Synthesize(t.leg.Context(), req.Text, ttsOptions(req, t.apiKey))
		if err != nil {
			deregisterLegPlayer(legID, ttsID)
			s.Bus.Publish(events.TTSError, &events.TTSErrorData{
				LegRoomScope: events.LegRoomScope{LegID: legID, AppID: t.leg.AppID()},
				TTSID:        ttsID,
				Error:        err.Error(),
				Category:     string(tts.Categorize(err)),
			})
			return
		}
		defer result.Audio.Close()

		s.playLegTTSAudio(t, ttsID, player, result.Audio, result.MimeType)
	}()

	return &TTSStartResult{TTSID: ttsID, Status: "playing"}, nil
}

func ttsOptions(req TTSRequest, apiKey string) tts.Options {
	return tts.Options{
		Voice:    req.Voice,
		ModelID:  req.ModelID,
		Language: req.Language,
		Prompt:   req.Prompt,
		APIKey:   apiKey,
	}
}

func (s *Server) ttsLeg(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req TTSRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	res, err := s.doLegTTS(id, req)
	if err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) doRoomTTS(roomID string, req TTSRequest) (*TTSStartResult, error) {
	rm, ok := s.RoomMgr.Get(roomID)
	if !ok {
		return nil, newAPIError(http.StatusNotFound, "room not found")
	}
	provider, apiKey := s.resolveTTSProvider(req)
	if provider == nil {
		providerName := req.Provider
		if providerName == "" {
			providerName = "elevenlabs"
		}
		return nil, newAPIError(http.StatusServiceUnavailable, "no %s API key provided", providerName)
	}
	if req.Text == "" {
		return nil, newAPIError(http.StatusBadRequest, "text is required")
	}
	if req.Voice == "" {
		return nil, newAPIError(http.StatusBadRequest, "voice is required")
	}
	if req.Volume < -8 || req.Volume > 8 {
		return nil, newAPIError(http.StatusBadRequest, "volume must be between -8 and 8")
	}
	parts := rm.Participants()
	if len(parts) == 0 {
		return nil, newAPIError(http.StatusConflict, "room has no participants")
	}

	id := roomID

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
				Category:     string(tts.Categorize(err)),
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
				Category:     string(tts.CategoryPlayback),
			})
		} else {
			s.Bus.Publish(events.TTSFinished, &events.TTSFinishedData{
				LegRoomScope: events.LegRoomScope{RoomID: id, AppID: roomAppID},
				TTSID:        ttsID,
				Reason:       playbackReason(playErr),
				PlayedMs:     player.PlayedMillis(),
			})
		}
	}()

	return &TTSStartResult{TTSID: ttsID, Status: "playing"}, nil
}

func (s *Server) ttsRoom(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req TTSRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	res, err := s.doRoomTTS(id, req)
	if err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
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
	// Retry is inner and the cache is outer: a cache hit never enters the
	// retry loop, so a cached utterance costs zero upstream calls.
	//
	// The nil check matters: without it every caller would get a non-nil
	// wrapper, and the "no API key configured -> 503" guards in doLegTTS and
	// doRoomTTS would stop firing when s.TTS itself is nil.
	if provider != nil {
		provider = tts.NewRetrying(provider, name, s.Log)
	}
	if s.TTSCache != nil {
		provider = s.TTSCache.WrapProvider(provider, name)
	}
	return provider, apiKey
}
