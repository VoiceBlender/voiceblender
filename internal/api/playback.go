package api

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/VoiceBlender/voiceblender/internal/mixer"
	"github.com/VoiceBlender/voiceblender/internal/playback"
	"github.com/VoiceBlender/voiceblender/internal/room"
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

	var req PlaybackRequest
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

	directWriter := l.AudioWriter()
	if directWriter == nil {
		writeError(w, http.StatusConflict, "leg has no audio writer")
		return
	}

	// Use a dynamic writer that checks per-frame whether the leg is in a
	// room. When in a room, frames are injected into the mixer (mixed with
	// room audio). When not in a room, frames go directly to the leg.
	// Playback always runs at the mixer's 16kHz rate; the dynamic writer
	// resamples to the leg's native rate when writing directly.
	writer := &legPlaybackWriter{
		legID:        id,
		leg:          l,
		directWriter: directWriter,
		roomMgr:      s.RoomMgr,
	}

	legPlayers.Lock()
	if legPlayers.m[id] == nil {
		legPlayers.m[id] = make(map[string]*playback.Player)
	}
	legPlayers.m[id][playbackID] = player
	legPlayers.Unlock()

	playRate := uint32(mixer.DefaultSampleRate)
	if roomID := l.RoomID(); roomID != "" {
		if rm, ok := s.RoomMgr.Get(roomID); ok {
			playRate = uint32(rm.Mixer().SampleRate())
		}
	}

	appID := l.AppID()
	player.OnStart(func() {
		s.Bus.Publish(events.PlaybackStarted, &events.PlaybackStartedData{
			LegRoomScope: events.LegRoomScope{LegID: id, AppID: appID},
			PlaybackID:   playbackID,
		})
	})
	go func() {
		var err error
		if req.Tone != "" {
			spec, ok := playback.LookupTone(req.Tone)
			if !ok {
				s.Bus.Publish(events.PlaybackError, &events.PlaybackErrorData{
					LegRoomScope: events.LegRoomScope{LegID: id, AppID: appID},
					PlaybackID:   playbackID,
					Error:        fmt.Sprintf("unknown tone %q, available: %s", req.Tone, strings.Join(playback.ToneNames(), ", ")),
				})
				return
			}
			toneReader := playback.NewToneReader(spec, int(playRate))
			err = player.PlayReaderAtRate(l.Context(), writer, toneReader, fmt.Sprintf("audio/pcm;rate=%d", playRate), playRate)
		} else {
			err = player.PlayAtRate(l.Context(), writer, req.URL, req.MimeType, playRate, req.Repeat)
		}
		legPlayers.Lock()
		delete(legPlayers.m[id], playbackID)
		if len(legPlayers.m[id]) == 0 {
			delete(legPlayers.m, id)
		}
		legPlayers.Unlock()
		if err != nil && err != context.Canceled {
			s.Bus.Publish(events.PlaybackError, &events.PlaybackErrorData{
				LegRoomScope: events.LegRoomScope{LegID: id, AppID: appID},
				PlaybackID:   playbackID,
				Error:        err.Error(),
			})
		} else {
			s.Bus.Publish(events.PlaybackFinished, &events.PlaybackFinishedData{
				LegRoomScope: events.LegRoomScope{LegID: id, AppID: appID},
				PlaybackID:   playbackID,
			})
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
	if !ok {
		legPlayers.Unlock()
		writeError(w, http.StatusNotFound, "no playback in progress")
		return
	}
	delete(players, playbackID)
	if len(players) == 0 {
		delete(legPlayers.m, id)
	}
	legPlayers.Unlock()
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

	var req PlaybackRequest
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

	roomAppID := rm.AppID
	player.OnStart(func() {
		s.Bus.Publish(events.PlaybackStarted, &events.PlaybackStartedData{
			LegRoomScope: events.LegRoomScope{RoomID: id, AppID: roomAppID},
			PlaybackID:   playbackID,
		})
	})

	go func() {
		var err error
		if req.Tone != "" {
			spec, ok := playback.LookupTone(req.Tone)
			if !ok {
				pw.Close()
				rm.Mixer().RemoveParticipant(playbackID)
				s.Bus.Publish(events.PlaybackError, &events.PlaybackErrorData{
					LegRoomScope: events.LegRoomScope{RoomID: id, AppID: roomAppID},
					PlaybackID:   playbackID,
					Error:        fmt.Sprintf("unknown tone %q, available: %s", req.Tone, strings.Join(playback.ToneNames(), ", ")),
				})
				return
			}
			roomRate := rm.Mixer().SampleRate()
			toneReader := playback.NewToneReader(spec, roomRate)
			err = player.PlayReaderAtRate(parts[0].Context(), pw, toneReader, fmt.Sprintf("audio/pcm;rate=%d", roomRate), uint32(roomRate))
		} else {
			err = player.PlayAtRate(parts[0].Context(), pw, req.URL, req.MimeType, uint32(rm.Mixer().SampleRate()), req.Repeat)
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
			s.Bus.Publish(events.PlaybackError, &events.PlaybackErrorData{
				LegRoomScope: events.LegRoomScope{RoomID: id, AppID: roomAppID},
				PlaybackID:   playbackID,
				Error:        err.Error(),
			})
		} else {
			s.Bus.Publish(events.PlaybackFinished, &events.PlaybackFinishedData{
				LegRoomScope: events.LegRoomScope{RoomID: id, AppID: roomAppID},
				PlaybackID:   playbackID,
			})
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
	if !ok {
		roomPlayers.Unlock()
		writeError(w, http.StatusNotFound, "no playback in progress")
		return
	}
	delete(players, playbackID)
	if len(players) == 0 {
		delete(roomPlayers.m, id)
	}
	roomPlayers.Unlock()
	p.Stop()
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}

func (s *Server) volumePlayLeg(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	playbackID := chi.URLParam(r, "playbackID")
	var req VolumeRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.Volume < -8 || req.Volume > 8 {
		writeError(w, http.StatusBadRequest, "volume must be between -8 and 8")
		return
	}
	legPlayers.Lock()
	p, ok := legPlayers.m[id][playbackID]
	legPlayers.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "playback not found")
		return
	}
	p.SetVolume(req.Volume)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) volumePlayRoom(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	playbackID := chi.URLParam(r, "playbackID")
	var req VolumeRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.Volume < -8 || req.Volume > 8 {
		writeError(w, http.StatusBadRequest, "volume must be between -8 and 8")
		return
	}
	roomPlayers.Lock()
	p, ok := roomPlayers.m[id][playbackID]
	roomPlayers.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "playback not found")
		return
	}
	p.SetVolume(req.Volume)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// legPlaybackWriter routes playback PCM frames dynamically based on
// whether the leg is currently in a room. Frames arrive at 16kHz (mixer
// rate).
//
//   - In a room: writes to the mixer's per-participant inject channel so
//     the playback audio is mixed with room audio.
//   - Not in a room: resamples to the leg's native rate and writes
//     directly to the leg's AudioWriter.
type legPlaybackWriter struct {
	legID        string
	leg          leg.Leg
	directWriter io.Writer // leg.AudioWriter(), captured once
	roomMgr      *room.Manager
}

func (w *legPlaybackWriter) Write(p []byte) (int, error) {
	roomID := w.leg.RoomID()
	if roomID != "" {
		if rm, ok := w.roomMgr.Get(roomID); ok {
			injW := rm.Mixer().InjectWriter(w.legID)
			if injW != nil {
				return injW.Write(p)
			}
		}
	}
	// Not in a room — resample from 16kHz to leg's native rate and write.
	legRate := uint32(w.leg.SampleRate())
	mixRate := uint32(mixer.DefaultSampleRate)
	if legRate == mixRate {
		return w.directWriter.Write(p)
	}
	// Resample: decode 16-bit LE samples, linear interpolation, re-encode.
	srcSamples := len(p) / 2
	src := make([]int16, srcSamples)
	for i := 0; i < srcSamples; i++ {
		src[i] = int16(binary.LittleEndian.Uint16(p[i*2:]))
	}
	ratio := float64(mixRate) / float64(legRate)
	outLen := int(float64(srcSamples) / ratio)
	out := make([]byte, outLen*2)
	for i := 0; i < outLen; i++ {
		srcPos := float64(i) * ratio
		idx := int(srcPos)
		frac := srcPos - float64(idx)
		var s int16
		if idx+1 < srcSamples {
			s0 := int32(src[idx])
			s1 := int32(src[idx+1])
			s = int16(s0 + int32(float64(s1-s0)*frac))
		} else if idx < srcSamples {
			s = src[idx]
		}
		binary.LittleEndian.PutUint16(out[i*2:], uint16(s))
	}
	return w.directWriter.Write(out)
}
