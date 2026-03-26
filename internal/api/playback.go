package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/csiwek/VoiceBlender/internal/events"
	"github.com/csiwek/VoiceBlender/internal/playback"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// playbackState tracks per-leg and per-room playback players.
// Nested map: entity_id → playback_id → *Player
var (
	legPlayers = struct {
		sync.Mutex
		m map[string]map[string]*playback.Player
	}{m: make(map[string]map[string]*playback.Player)}

	roomPlayers = struct {
		sync.Mutex
		m map[string]map[string]*playback.Player
	}{m: make(map[string]map[string]*playback.Player)}
)

func (s *Server) playLeg(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	l, ok := s.LegMgr.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "leg not found")
		return
	}

	var req struct {
		URL      string `json:"url"`
		Tone     string `json:"tone"`
		MimeType string `json:"mime_type"`
		Repeat   int    `json:"repeat"`
		Volume   int    `json:"volume"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.URL != "" && req.Tone != "" {
		writeError(w, http.StatusBadRequest, "url and tone are mutually exclusive")
		return
	}
	if req.URL == "" && req.Tone == "" {
		writeError(w, http.StatusBadRequest, "url or tone is required")
		return
	}
	if req.Volume < -8 || req.Volume > 8 {
		writeError(w, http.StatusBadRequest, "volume must be between -8 and 8")
		return
	}

	playbackID := "pb-" + uuid.New().String()[:8]
	player := playback.NewPlayer(s.Log)
	player.SetVolume(req.Volume)

	// Write decoded 8kHz PCM directly to the leg's AudioWriter
	writer := l.AudioWriter()
	if writer == nil {
		writeError(w, http.StatusConflict, "leg has no audio writer")
		return
	}

	legPlayers.Lock()
	if legPlayers.m[id] == nil {
		legPlayers.m[id] = make(map[string]*playback.Player)
	}
	legPlayers.m[id][playbackID] = player
	legPlayers.Unlock()

	player.OnStart(func() {
		s.Bus.Publish(events.PlaybackStarted, map[string]interface{}{"leg_id": id, "playback_id": playbackID})
	})
	go func() {
		var err error
		if req.Tone != "" {
			spec, ok := playback.LookupTone(req.Tone)
			if !ok {
				s.Bus.Publish(events.PlaybackError, map[string]interface{}{
					"leg_id": id, "playback_id": playbackID,
					"error": fmt.Sprintf("unknown tone %q, available: %s", req.Tone, strings.Join(playback.ToneNames(), ", ")),
				})
				return
			}
			rate := uint32(l.SampleRate())
			toneReader := playback.NewToneReader(spec, int(rate))
			err = player.PlayReaderAtRate(l.Context(), writer, toneReader, fmt.Sprintf("audio/pcm;rate=%d", rate), rate)
		} else {
			err = player.PlayAtRate(l.Context(), writer, req.URL, req.MimeType, uint32(l.SampleRate()), req.Repeat)
		}
		legPlayers.Lock()
		delete(legPlayers.m[id], playbackID)
		if len(legPlayers.m[id]) == 0 {
			delete(legPlayers.m, id)
		}
		legPlayers.Unlock()
		if err != nil && err != context.Canceled {
			s.Bus.Publish(events.PlaybackError, map[string]interface{}{"leg_id": id, "playback_id": playbackID, "error": err.Error()})
		} else {
			s.Bus.Publish(events.PlaybackFinished, map[string]interface{}{"leg_id": id, "playback_id": playbackID})
		}
	}()

	writeJSON(w, http.StatusOK, map[string]string{"playback_id": playbackID, "status": "playing"})
}

func (s *Server) stopPlayLeg(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	playbackID := chi.URLParam(r, "playbackID")
	legPlayers.Lock()
	players, ok := legPlayers.m[id]
	if !ok {
		legPlayers.Unlock()
		writeError(w, http.StatusNotFound, "no playback in progress")
		return
	}
	p, ok := players[playbackID]
	legPlayers.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "no playback in progress")
		return
	}
	p.Stop()
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}

func (s *Server) playRoom(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	rm, ok := s.RoomMgr.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "room not found")
		return
	}

	var req struct {
		URL      string `json:"url"`
		Tone     string `json:"tone"`
		MimeType string `json:"mime_type"`
		Repeat   int    `json:"repeat"`
		Volume   int    `json:"volume"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.URL != "" && req.Tone != "" {
		writeError(w, http.StatusBadRequest, "url and tone are mutually exclusive")
		return
	}
	if req.URL == "" && req.Tone == "" {
		writeError(w, http.StatusBadRequest, "url or tone is required")
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

	// Create a pipe: player writes 16kHz PCM into pw, mixer reads from pr.
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

	player.OnStart(func() {
		s.Bus.Publish(events.PlaybackStarted, map[string]interface{}{"room_id": id, "playback_id": playbackID})
	})

	go func() {
		var err error
		if req.Tone != "" {
			spec, ok := playback.LookupTone(req.Tone)
			if !ok {
				pw.Close()
				rm.Mixer().RemoveParticipant(playbackID)
				s.Bus.Publish(events.PlaybackError, map[string]interface{}{
					"room_id": id, "playback_id": playbackID,
					"error": fmt.Sprintf("unknown tone %q, available: %s", req.Tone, strings.Join(playback.ToneNames(), ", ")),
				})
				return
			}
			toneReader := playback.NewToneReader(spec, 16000)
			err = player.PlayReader(parts[0].Context(), pw, toneReader, "audio/pcm;rate=16000")
		} else {
			// Play outputs 16kHz PCM (mixer native rate) into the pipe
			err = player.Play(parts[0].Context(), pw, req.URL, req.MimeType, req.Repeat)
		}
		pw.Close()
		rm.Mixer().RemoveParticipant(playbackID)
		roomPlayers.Lock()
		delete(roomPlayers.m[id], playbackID)
		if len(roomPlayers.m[id]) == 0 {
			delete(roomPlayers.m, id)
		}
		roomPlayers.Unlock()
		if err != nil && err != context.Canceled {
			s.Log.Debug("room playback error", "room_id", id, "error", err)
			s.Bus.Publish(events.PlaybackError, map[string]interface{}{"room_id": id, "playback_id": playbackID, "error": err.Error()})
		} else {
			s.Bus.Publish(events.PlaybackFinished, map[string]interface{}{"room_id": id, "playback_id": playbackID})
		}
	}()

	writeJSON(w, http.StatusOK, map[string]string{"playback_id": playbackID, "status": "playing"})
}

func (s *Server) stopPlayRoom(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	playbackID := chi.URLParam(r, "playbackID")
	roomPlayers.Lock()
	players, ok := roomPlayers.m[id]
	if !ok {
		roomPlayers.Unlock()
		writeError(w, http.StatusNotFound, "no playback in progress")
		return
	}
	p, ok := players[playbackID]
	roomPlayers.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "no playback in progress")
		return
	}
	p.Stop()
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}
