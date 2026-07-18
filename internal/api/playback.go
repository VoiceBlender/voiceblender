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

// playbackReason classifies why a playback or TTS utterance stopped, for the
// finished events. It is only called on the non-error path, which is reached for
// a nil error (the audio reached its end) or for context.Canceled. Anything that
// is not a clean end is reported as "stopped" — an app-initiated stop and a leg
// teardown both cancel the same context and cannot be told apart here;
// consumers use the co-emitted leg.disconnected event for that.
func playbackReason(err error) string {
	if err == nil {
		return "completed"
	}
	return "stopped"
}

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

// PlaybackStartResult is the success payload for starting a playback on a
// leg or room. The same shape is returned by leg_tts and room_tts (with
// "tts_id" semantics).
type PlaybackStartResult struct {
	PlaybackID string `json:"playback_id"`
	Status     string `json:"status"`
}

// PlaybackStopResult is the success payload for stopping a playback.
type PlaybackStopResult struct {
	Status string `json:"status"`
}

func (s *Server) doStartLegPlay(legID string, req PlaybackRequest) (*PlaybackStartResult, error) {
	l, ok := s.LegMgr.Get(legID)
	if !ok {
		return nil, newAPIError(http.StatusNotFound, "leg not found")
	}
	if req.URL != "" && req.Tone != "" {
		return nil, newAPIError(http.StatusBadRequest, "url and tone are mutually exclusive")
	}
	if req.URL == "" && req.Tone == "" {
		return nil, newAPIError(http.StatusBadRequest, "url or tone is required")
	}
	if req.Volume < -8 || req.Volume > 8 {
		return nil, newAPIError(http.StatusBadRequest, "volume must be between -8 and 8")
	}

	directWriter := l.AudioWriter()
	if directWriter == nil {
		return nil, newAPIError(http.StatusConflict, "leg has no audio writer")
	}

	playbackID := "pb-" + uuid.New().String()[:8]
	player := playback.NewPlayer(s.Log)
	player.SetVolume(req.Volume)

	legPlayers.Lock()
	if legPlayers.m[legID] == nil {
		legPlayers.m[legID] = make(map[string]*playback.Player)
	}
	legPlayers.m[legID][playbackID] = player
	legPlayers.Unlock()

	playRate := uint32(mixer.DefaultSampleRate)
	if roomID := l.RoomID(); roomID != "" {
		if rm, ok := s.RoomMgr.Get(roomID); ok {
			playRate = uint32(rm.Mixer().SampleRate())
		}
	}

	writer := &legPlaybackWriter{
		legID:        legID,
		leg:          l,
		directWriter: directWriter,
		roomMgr:      s.RoomMgr,
		srcRate:      playRate,
	}

	appID := l.AppID()
	player.OnStart(func() {
		s.Bus.Publish(events.PlaybackStarted, &events.PlaybackStartedData{
			LegRoomScope: events.LegRoomScope{LegID: legID, AppID: appID},
			PlaybackID:   playbackID,
		})
	})
	go func() {
		var err error
		if req.Tone != "" {
			spec, ok := playback.LookupTone(req.Tone)
			if !ok {
				s.Bus.Publish(events.PlaybackError, &events.PlaybackErrorData{
					LegRoomScope: events.LegRoomScope{LegID: legID, AppID: appID},
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
		delete(legPlayers.m[legID], playbackID)
		if len(legPlayers.m[legID]) == 0 {
			delete(legPlayers.m, legID)
		}
		legPlayers.Unlock()
		if err != nil && err != context.Canceled {
			s.Bus.Publish(events.PlaybackError, &events.PlaybackErrorData{
				LegRoomScope: events.LegRoomScope{LegID: legID, AppID: appID},
				PlaybackID:   playbackID,
				Error:        err.Error(),
			})
		} else {
			s.Bus.Publish(events.PlaybackFinished, &events.PlaybackFinishedData{
				LegRoomScope: events.LegRoomScope{LegID: legID, AppID: appID},
				PlaybackID:   playbackID,
				Reason:       playbackReason(err),
				PlayedMs:     player.PlayedMillis(),
			})
		}
	}()

	return &PlaybackStartResult{PlaybackID: playbackID, Status: "playing"}, nil
}

func (s *Server) playLeg(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req PlaybackRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	res, err := s.doStartLegPlay(id, req)
	if err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) doStopLegPlay(legID, playbackID string) (*PlaybackStopResult, error) {
	legPlayers.Lock()
	players, ok := legPlayers.m[legID]
	if !ok {
		legPlayers.Unlock()
		return nil, newAPIError(http.StatusNotFound, "no playback in progress")
	}
	p, ok := players[playbackID]
	if !ok {
		legPlayers.Unlock()
		return nil, newAPIError(http.StatusNotFound, "no playback in progress")
	}
	delete(players, playbackID)
	if len(players) == 0 {
		delete(legPlayers.m, legID)
	}
	legPlayers.Unlock()
	p.Stop()
	return &PlaybackStopResult{Status: "stopped"}, nil
}

func (s *Server) stopPlayLeg(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	playbackID := chi.URLParam(r, "playbackID")
	res, err := s.doStopLegPlay(id, playbackID)
	if err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) doStartRoomPlay(roomID string, req PlaybackRequest) (*PlaybackStartResult, error) {
	rm, ok := s.RoomMgr.Get(roomID)
	if !ok {
		return nil, newAPIError(http.StatusNotFound, "room not found")
	}
	if req.URL != "" && req.Tone != "" {
		return nil, newAPIError(http.StatusBadRequest, "url and tone are mutually exclusive")
	}
	if req.URL == "" && req.Tone == "" {
		return nil, newAPIError(http.StatusBadRequest, "url or tone is required")
	}
	if req.Volume < -8 || req.Volume > 8 {
		return nil, newAPIError(http.StatusBadRequest, "volume must be between -8 and 8")
	}
	parts := rm.Participants()
	if len(parts) == 0 {
		return nil, newAPIError(http.StatusConflict, "room has no participants")
	}

	playbackID := "pb-" + uuid.New().String()[:8]
	pr, pw := io.Pipe()
	rm.Mixer().AddPlaybackSource(playbackID, pr)

	player := playback.NewPlayer(s.Log)
	player.SetVolume(req.Volume)
	roomPlayers.Lock()
	if roomPlayers.m[roomID] == nil {
		roomPlayers.m[roomID] = make(map[string]*playback.Player)
	}
	roomPlayers.m[roomID][playbackID] = player
	roomPlayers.Unlock()

	roomAppID := rm.AppID
	player.OnStart(func() {
		s.Bus.Publish(events.PlaybackStarted, &events.PlaybackStartedData{
			LegRoomScope: events.LegRoomScope{RoomID: roomID, AppID: roomAppID},
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
					LegRoomScope: events.LegRoomScope{RoomID: roomID, AppID: roomAppID},
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
		delete(roomPlayers.m[roomID], playbackID)
		if len(roomPlayers.m[roomID]) == 0 {
			delete(roomPlayers.m, roomID)
		}
		roomPlayers.Unlock()
		if err != nil && err != context.Canceled {
			s.Log.Debug("room playback error", "room_id", roomID, "error", err)
			s.Bus.Publish(events.PlaybackError, &events.PlaybackErrorData{
				LegRoomScope: events.LegRoomScope{RoomID: roomID, AppID: roomAppID},
				PlaybackID:   playbackID,
				Error:        err.Error(),
			})
		} else {
			s.Bus.Publish(events.PlaybackFinished, &events.PlaybackFinishedData{
				LegRoomScope: events.LegRoomScope{RoomID: roomID, AppID: roomAppID},
				PlaybackID:   playbackID,
				Reason:       playbackReason(err),
				PlayedMs:     player.PlayedMillis(),
			})
		}
	}()

	return &PlaybackStartResult{PlaybackID: playbackID, Status: "playing"}, nil
}

func (s *Server) playRoom(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req PlaybackRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	res, err := s.doStartRoomPlay(id, req)
	if err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) doStopRoomPlay(roomID, playbackID string) (*PlaybackStopResult, error) {
	roomPlayers.Lock()
	players, ok := roomPlayers.m[roomID]
	if !ok {
		roomPlayers.Unlock()
		return nil, newAPIError(http.StatusNotFound, "no playback in progress")
	}
	p, ok := players[playbackID]
	if !ok {
		roomPlayers.Unlock()
		return nil, newAPIError(http.StatusNotFound, "no playback in progress")
	}
	delete(players, playbackID)
	if len(players) == 0 {
		delete(roomPlayers.m, roomID)
	}
	roomPlayers.Unlock()
	p.Stop()
	return &PlaybackStopResult{Status: "stopped"}, nil
}

func (s *Server) stopPlayRoom(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	playbackID := chi.URLParam(r, "playbackID")
	res, err := s.doStopRoomPlay(id, playbackID)
	if err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) doVolumeLegPlay(legID, playbackID string, volume int) error {
	if volume < -8 || volume > 8 {
		return newAPIError(http.StatusBadRequest, "volume must be between -8 and 8")
	}
	legPlayers.Lock()
	p, ok := legPlayers.m[legID][playbackID]
	legPlayers.Unlock()
	if !ok {
		return newAPIError(http.StatusNotFound, "playback not found")
	}
	p.SetVolume(volume)
	return nil
}

func (s *Server) volumePlayLeg(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	playbackID := chi.URLParam(r, "playbackID")
	var req VolumeRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if err := s.doVolumeLegPlay(id, playbackID, req.Volume); err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) doVolumeRoomPlay(roomID, playbackID string, volume int) error {
	if volume < -8 || volume > 8 {
		return newAPIError(http.StatusBadRequest, "volume must be between -8 and 8")
	}
	roomPlayers.Lock()
	p, ok := roomPlayers.m[roomID][playbackID]
	roomPlayers.Unlock()
	if !ok {
		return newAPIError(http.StatusNotFound, "playback not found")
	}
	p.SetVolume(volume)
	return nil
}

func (s *Server) volumePlayRoom(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	playbackID := chi.URLParam(r, "playbackID")
	var req VolumeRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if err := s.doVolumeRoomPlay(id, playbackID, req.Volume); err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// legPlaybackWriter routes playback PCM frames dynamically based on
// whether the leg is currently in a room. The producer writes frames at
// srcRate; the writer resamples to the destination's rate on every
// Write, which is what lets a leg join/leave a differently-rated room
// mid-stream without pitch shift.
//
//   - In a room: writes to the mixer's per-participant inject channel,
//     resampling srcRate → room mixer rate. The mixer's inject path
//     mixes raw samples without rate conversion, so the bytes must
//     already be at the room's rate.
//   - Not in a room: writes directly to the leg's AudioWriter,
//     resampling srcRate → leg native rate.
type legPlaybackWriter struct {
	legID        string
	leg          leg.Leg
	directWriter io.Writer // leg.AudioWriter(), captured once
	roomMgr      *room.Manager
	srcRate      uint32 // rate of bytes arriving at Write()
}

func (w *legPlaybackWriter) Write(p []byte) (int, error) {
	roomID := w.leg.RoomID()
	if roomID != "" {
		if rm, ok := w.roomMgr.Get(roomID); ok {
			injW := rm.Mixer().InjectWriter(w.legID)
			if injW != nil {
				dstRate := uint32(rm.Mixer().SampleRate())
				if w.srcRate == dstRate {
					return injW.Write(p)
				}
				if _, err := injW.Write(resamplePCM16(p, w.srcRate, dstRate)); err != nil {
					return 0, err
				}
				return len(p), nil
			}
		}
	}
	legRate := uint32(w.leg.SampleRate())
	if w.srcRate == legRate {
		return w.directWriter.Write(p)
	}
	if _, err := w.directWriter.Write(resamplePCM16(p, w.srcRate, legRate)); err != nil {
		return 0, err
	}
	return len(p), nil
}

// resamplePCM16 resamples a mono 16-bit LE PCM buffer using linear
// interpolation. Stateless: the input must be a whole number of samples
// and is assumed to be a complete frame boundary (the player writes
// one ptime frame per call), so no carry-over state is needed.
func resamplePCM16(p []byte, srcRate, dstRate uint32) []byte {
	if srcRate == dstRate || len(p) < 2 {
		return p
	}
	srcSamples := len(p) / 2
	src := make([]int16, srcSamples)
	for i := 0; i < srcSamples; i++ {
		src[i] = int16(binary.LittleEndian.Uint16(p[i*2:]))
	}
	ratio := float64(srcRate) / float64(dstRate)
	outLen := int(float64(srcSamples) / ratio)
	if outLen < 1 {
		outLen = 1
	}
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
	return out
}
