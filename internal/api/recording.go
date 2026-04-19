package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/VoiceBlender/voiceblender/internal/recording"
	"github.com/VoiceBlender/voiceblender/internal/storage"
	"github.com/go-chi/chi/v5"
)

// legRecordInfo tracks state needed to cleanly stop a stereo leg recording.
type legRecordInfo struct {
	roomID  string
	pipes   []*pipeWriter
	storage storage.Backend
}

// multiChannelState tracks per-participant recording state for a room.
// Each participant gets a mono WAV recorded via the mixer's recordTap.
// At stop time, all per-participant WAVs are merged into a single
// multi-channel WAV with silence padding for join/leave time alignment.
type multiChannelState struct {
	mu         sync.Mutex
	active     bool
	paused     bool
	startTime  time.Time
	sampleRate int
	storage    storage.Backend
	dir        string
	recorders  map[string]*recording.Recorder // legID → recorder
	pipes      map[string]*pipeWriter         // legID → pipe writer (to close on stop)
	files      map[string]string              // legID → local WAV path (finalized)
	// Channel assignment — preserves order for deterministic channel mapping.
	participantOrder []string
	// Timing — join/leave offsets relative to startTime.
	joinOffsets  map[string]time.Duration
	leaveOffsets map[string]time.Duration
	log          *slog.Logger
}

// startLeg begins recording a single participant's audio via the mixer's recordTap.
func (mc *multiChannelState) startLeg(legID string, m mixerIface, dir string) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	if !mc.active {
		return
	}
	if _, exists := mc.recorders[legID]; exists {
		return
	}

	pr, pw := createPipe()
	m.SetParticipantRecordTap(legID, pw)

	rec := recording.NewRecorder(mc.log)
	fpath, err := rec.StartAt(context.Background(), pr, dir, uint32(mc.sampleRate))
	if err != nil {
		mc.log.Error("multi-channel: failed to start per-leg recording", "leg_id", legID, "error", err)
		m.ClearParticipantRecordTap(legID)
		pw.Close()
		return
	}

	// If the room recording is currently paused, a late-joining participant
	// must start paused too so their audio isn't captured while sensitive
	// data is being handled.
	if mc.paused {
		rec.Pause()
	}

	mc.recorders[legID] = rec
	mc.pipes[legID] = pw
	mc.participantOrder = append(mc.participantOrder, legID)
	mc.joinOffsets[legID] = time.Since(mc.startTime)
	mc.log.Info("multi-channel: started per-leg recording", "leg_id", legID, "file", fpath)
}

// stopLeg stops recording for a single participant and stores the finalized local path.
func (mc *multiChannelState) stopLeg(legID string, m mixerIface) {
	mc.mu.Lock()
	rec, ok := mc.recorders[legID]
	if !ok {
		mc.mu.Unlock()
		return
	}
	pw := mc.pipes[legID]
	delete(mc.recorders, legID)
	delete(mc.pipes, legID)
	mc.leaveOffsets[legID] = time.Since(mc.startTime)
	mc.mu.Unlock()

	m.ClearParticipantRecordTap(legID)
	if pw != nil {
		pw.Close()
	}

	fpath := rec.Stop()
	rec.Wait()

	mc.mu.Lock()
	mc.files[legID] = fpath
	mc.mu.Unlock()
	mc.log.Info("multi-channel: stopped per-leg recording", "leg_id", legID, "file", fpath)
}

// stopAll stops all per-participant recordings, merges into a single
// multi-channel WAV, uploads if needed, and returns the result.
func (mc *multiChannelState) stopAll(m mixerIface) (*recording.MultiChannelResult, error) {
	mc.mu.Lock()
	mc.active = false
	totalDuration := time.Since(mc.startTime)
	// Snapshot the leg IDs still recording.
	legIDs := make([]string, 0, len(mc.recorders))
	for id := range mc.recorders {
		legIDs = append(legIDs, id)
	}
	mc.mu.Unlock()

	// Stop any still-recording participants.
	for _, id := range legIDs {
		mc.stopLeg(id, m)
	}

	mc.mu.Lock()
	// Build merge inputs in channel order.
	inputs := make([]recording.MultiChannelInput, len(mc.participantOrder))
	for i, legID := range mc.participantOrder {
		inputs[i] = recording.MultiChannelInput{
			LegID:      legID,
			FilePath:   mc.files[legID],
			JoinOffset: mc.joinOffsets[legID],
		}
	}
	mc.mu.Unlock()

	result, err := recording.MergeMultiChannel(mc.dir, inputs, totalDuration, mc.sampleRate)
	if err != nil {
		mc.log.Error("multi-channel: merge failed", "error", err)
		return nil, err
	}

	// Upload the merged file if storage backend is set.
	if mc.storage != nil {
		loc, uploadErr := mc.storage.Upload(context.Background(), result.FilePath)
		if uploadErr != nil {
			mc.log.Error("multi-channel: storage upload failed", "error", uploadErr)
		} else {
			result.FilePath = loc
		}
	}

	// Clean up intermediate per-participant WAV files.
	mc.mu.Lock()
	for _, fpath := range mc.files {
		os.Remove(fpath)
	}
	mc.mu.Unlock()

	return result, nil
}

// mixerIface is the subset of mixer.Mixer methods used by multiChannelState,
// allowing for easier testing.
type mixerIface interface {
	SetParticipantRecordTap(id string, w io.Writer)
	ClearParticipantRecordTap(id string)
}

var (
	// roomMultiChannel tracks multi-channel recording state per room.
	roomMultiChannel = struct {
		sync.Mutex
		m map[string]*multiChannelState
	}{m: make(map[string]*multiChannelState)}

	legRecorders = struct {
		sync.Mutex
		m map[string]*recording.Recorder
	}{m: make(map[string]*recording.Recorder)}

	// legRecordState tracks which room a leg was in and the pipe writers
	// used for stereo recording, so we can clean up when stopping.
	legRecordState = struct {
		sync.Mutex
		m map[string]*legRecordInfo
	}{m: make(map[string]*legRecordInfo)}

	roomRecorders = struct {
		sync.Mutex
		m map[string]*recording.Recorder
	}{m: make(map[string]*recording.Recorder)}

	// roomRecordPipes tracks pipe writers for room recordings so we can
	// close them to unblock the recording goroutine on stop.
	roomRecordPipes = struct {
		sync.Mutex
		m map[string]*pipeWriter
	}{m: make(map[string]*pipeWriter)}

	// roomRecordStorage tracks the storage backend chosen for each room recording.
	roomRecordStorage = struct {
		sync.Mutex
		m map[string]storage.Backend
	}{m: make(map[string]storage.Backend)}
)

// resolveStorage returns the appropriate storage backend for the request.
// If the request includes per-request S3 config (s3_bucket), a new S3Backend
// is created on the fly. Otherwise, falls back to the server-level S3 backend.
func (s *Server) resolveStorage(req RecordRequest) (storage.Backend, error) {
	switch req.Storage {
	case "", "file":
		return storage.FileBackend{}, nil
	case "s3":
		// Per-request S3 config takes precedence.
		if req.S3Bucket != "" {
			region := req.S3Region
			if region == "" {
				region = "us-east-1"
			}
			backend, err := storage.NewS3Backend(context.Background(), storage.S3Config{
				Bucket:    req.S3Bucket,
				Region:    region,
				Endpoint:  req.S3Endpoint,
				Prefix:    req.S3Prefix,
				AccessKey: req.S3AccessKey,
				SecretKey: req.S3SecretKey,
			})
			if err != nil {
				return nil, fmt.Errorf("create S3 backend: %w", err)
			}
			return backend, nil
		}
		if s.S3 == nil {
			return nil, fmt.Errorf("S3 storage not configured: set S3_BUCKET env var or provide s3_bucket in request")
		}
		return s.S3, nil
	default:
		return nil, fmt.Errorf("unknown storage type: %s", req.Storage)
	}
}

func (s *Server) recordLeg(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	l, ok := s.LegMgr.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "leg not found")
		return
	}

	var req RecordRequest
	if r.Body != nil {
		json.NewDecoder(r.Body).Decode(&req) // ignore error; empty body is fine
	}

	backend, err := s.resolveStorage(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	rec := recording.NewRecorder(s.Log)
	var fpath string
	var recErr error

	// If the leg is in a room, record stereo: left=incoming audio, right=mixed-minus-self.
	if roomID := l.RoomID(); roomID != "" {
		rm, rmOK := s.RoomMgr.Get(roomID)
		if !rmOK {
			writeError(w, http.StatusConflict, "leg's room not found")
			return
		}

		leftPR, leftPW := createPipe()
		rightPR, rightPW := createPipe()

		mix := rm.Mixer()
		mix.SetParticipantTap(id, leftPW)
		mix.SetParticipantOutTap(id, rightPW)

		fpath, recErr = rec.StartStereo(l.Context(), leftPR, rightPR, s.Config.RecordingDir, uint32(rm.Mixer().SampleRate()))
		if recErr != nil {
			mix.ClearParticipantTap(id)
			mix.ClearParticipantOutTap(id)
			writeError(w, http.StatusInternalServerError, recErr.Error())
			return
		}

		legRecordState.Lock()
		legRecordState.m[id] = &legRecordInfo{
			roomID:  roomID,
			pipes:   []*pipeWriter{leftPW, rightPW},
			storage: backend,
		}
		legRecordState.Unlock()
	} else if sipLeg, ok := l.(*leg.SIPLeg); ok {
		// Standalone SIP leg — stereo via RTP taps:
		// left = incoming (what remote says), right = outgoing (what we send).
		leftPR, leftPW := createPipe()
		rightPR, rightPW := createPipe()

		sipLeg.SetInTap(leftPW)
		sipLeg.SetOutTap(rightPW)

		fpath, recErr = rec.StartStereo(l.Context(), leftPR, rightPR, s.Config.RecordingDir, uint32(l.SampleRate()))
		if recErr != nil {
			sipLeg.ClearInTap()
			sipLeg.ClearOutTap()
			writeError(w, http.StatusInternalServerError, recErr.Error())
			return
		}

		legRecordState.Lock()
		legRecordState.m[id] = &legRecordInfo{
			pipes:   []*pipeWriter{leftPW, rightPW},
			storage: backend,
		}
		legRecordState.Unlock()
	} else {
		// Non-SIP standalone leg — mono recording from the leg's audio reader.
		reader := l.AudioReader()
		if reader == nil {
			writeError(w, http.StatusConflict, "leg has no audio reader")
			return
		}
		fpath, recErr = rec.StartAt(l.Context(), reader, s.Config.RecordingDir, uint32(l.SampleRate()))
		if recErr != nil {
			writeError(w, http.StatusInternalServerError, recErr.Error())
			return
		}

		// Mono leg still needs storage tracking.
		legRecordState.Lock()
		legRecordState.m[id] = &legRecordInfo{
			storage: backend,
		}
		legRecordState.Unlock()
	}

	legRecorders.Lock()
	legRecorders.m[id] = rec
	legRecorders.Unlock()

	s.Bus.Publish(events.RecordingStarted, &events.RecordingStartedData{
		LegRoomScope: events.LegRoomScope{LegID: id, AppID: l.AppID()},
		File:         fpath,
	})
	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "recording", "file": fpath})
}

// stopLegRecording stops any active recording for the given leg, cleans up
// mixer taps and pipes, and emits recording.finished. Called from both the
// REST endpoint and cleanupLeg (on disconnect). Returns the file path and
// true if a recording was stopped.
func (s *Server) stopLegRecording(legID string) (string, bool) {
	legRecorders.Lock()
	rec, ok := legRecorders.m[legID]
	if ok {
		delete(legRecorders.m, legID)
	}
	legRecorders.Unlock()
	if !ok {
		return "", false
	}

	// Clear mixer taps and close pipes if this was a stereo (in-room) recording.
	legRecordState.Lock()
	info := legRecordState.m[legID]
	delete(legRecordState.m, legID)
	legRecordState.Unlock()

	if info != nil {
		if info.roomID != "" {
			// In-room recording: clear mixer taps.
			if rm, rmOK := s.RoomMgr.Get(info.roomID); rmOK {
				mix := rm.Mixer()
				mix.ClearParticipantTap(legID)
				mix.ClearParticipantOutTap(legID)
			}
		} else {
			// Standalone SIP leg: clear leg-level taps.
			if l, lOK := s.LegMgr.Get(legID); lOK {
				if sipLeg, ok := l.(*leg.SIPLeg); ok {
					sipLeg.ClearInTap()
					sipLeg.ClearOutTap()
				}
			}
		}
		// Close pipes to unblock the recording goroutine so enc.Close()
		// can finalize the WAV header.
		for _, pw := range info.pipes {
			pw.Close()
		}
	}

	fpath := rec.Stop()
	rec.Wait()

	// Upload to storage backend if not plain file.
	var backend storage.Backend
	if info != nil {
		backend = info.storage
	}
	location := fpath
	if backend != nil {
		loc, err := backend.Upload(context.Background(), fpath)
		if err != nil {
			s.Log.Error("storage upload failed", "leg_id", legID, "error", err)
			// Keep local file and use local path.
		} else {
			location = loc
		}
	}

	legAppID := ""
	if ll, ok := s.LegMgr.Get(legID); ok {
		legAppID = ll.AppID()
	}
	s.Bus.Publish(events.RecordingFinished, &events.RecordingFinishedData{
		LegRoomScope: events.LegRoomScope{LegID: legID, AppID: legAppID},
		File:         location,
	})
	return location, true
}

func (s *Server) stopRecordLeg(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	fpath, ok := s.stopLegRecording(id)
	if !ok {
		writeError(w, http.StatusNotFound, "no recording in progress")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "stopped", "file": fpath})
}

func (s *Server) pauseRecordLeg(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	legRecorders.Lock()
	rec, ok := legRecorders.m[id]
	legRecorders.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "no recording in progress")
		return
	}
	if !rec.Pause() {
		writeJSON(w, http.StatusOK, map[string]interface{}{"status": "already_paused"})
		return
	}
	legAppID := ""
	if ll, ok := s.LegMgr.Get(id); ok {
		legAppID = ll.AppID()
	}
	s.Bus.Publish(events.RecordingPaused, &events.RecordingPausedData{
		LegRoomScope: events.LegRoomScope{LegID: id, AppID: legAppID},
	})
	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "paused"})
}

func (s *Server) resumeRecordLeg(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	legRecorders.Lock()
	rec, ok := legRecorders.m[id]
	legRecorders.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "no recording in progress")
		return
	}
	if !rec.Resume() {
		writeJSON(w, http.StatusOK, map[string]interface{}{"status": "not_paused"})
		return
	}
	legAppID := ""
	if ll, ok := s.LegMgr.Get(id); ok {
		legAppID = ll.AppID()
	}
	s.Bus.Publish(events.RecordingResumed, &events.RecordingResumedData{
		LegRoomScope: events.LegRoomScope{LegID: id, AppID: legAppID},
	})
	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "resumed"})
}

func (s *Server) recordRoom(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	rm, ok := s.RoomMgr.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "room not found")
		return
	}

	var req RecordRequest
	if r.Body != nil {
		json.NewDecoder(r.Body).Decode(&req)
	}

	backend, err := s.resolveStorage(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	parts := rm.Participants()
	if len(parts) == 0 {
		writeError(w, http.StatusConflict, "room has no participants")
		return
	}

	// Use the mixer tap via a pipe for room recording (full mix, always started)
	pr, pw := createPipe()
	rm.Mixer().SetTap(pw)

	rec := recording.NewRecorder(s.Log)
	fpath, err := rec.StartAt(parts[0].Context(), pr, s.Config.RecordingDir, uint32(rm.Mixer().SampleRate()))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	roomRecorders.Lock()
	roomRecorders.m[id] = rec
	roomRecorders.Unlock()

	roomRecordPipes.Lock()
	roomRecordPipes.m[id] = pw
	roomRecordPipes.Unlock()

	roomRecordStorage.Lock()
	roomRecordStorage.m[id] = backend
	roomRecordStorage.Unlock()

	// Multi-channel: start per-participant recordings.
	if req.MultiChannel {
		mc := &multiChannelState{
			active:       true,
			startTime:    time.Now(),
			sampleRate:   rm.Mixer().SampleRate(),
			storage:      backend,
			dir:          s.Config.RecordingDir,
			recorders:    make(map[string]*recording.Recorder),
			pipes:        make(map[string]*pipeWriter),
			files:        make(map[string]string),
			joinOffsets:  make(map[string]time.Duration),
			leaveOffsets: make(map[string]time.Duration),
			log:          s.Log,
		}
		roomMultiChannel.Lock()
		roomMultiChannel.m[id] = mc
		roomMultiChannel.Unlock()

		mix := rm.Mixer()
		for _, p := range parts {
			mc.startLeg(p.ID(), mix, s.Config.RecordingDir)
		}
	}

	s.Bus.Publish(events.RecordingStarted, &events.RecordingStartedData{
		LegRoomScope: events.LegRoomScope{RoomID: id, AppID: rm.AppID},
		File:         fpath,
	})
	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "recording", "file": fpath})
}

// cleanupRoomRecording stops all recording activity for a room and returns
// the result. Returns nil if no recording was in progress.
func (s *Server) cleanupRoomRecording(id string) (location string, mcResult *recording.MultiChannelResult, ok bool) {
	rm, rmOK := s.RoomMgr.Get(id)
	if rmOK {
		rm.Mixer().SetTap(nil)
	}

	// Stop multi-channel per-participant recordings first.
	roomMultiChannel.Lock()
	mc := roomMultiChannel.m[id]
	delete(roomMultiChannel.m, id)
	roomMultiChannel.Unlock()
	if mc != nil && rm != nil {
		result, err := mc.stopAll(rm.Mixer())
		if err != nil {
			s.Log.Error("multi-channel merge failed", "room_id", id, "error", err)
		} else {
			mcResult = result
		}
	}

	roomRecordPipes.Lock()
	pw := roomRecordPipes.m[id]
	delete(roomRecordPipes.m, id)
	roomRecordPipes.Unlock()
	if pw != nil {
		pw.Close()
	}

	roomRecorders.Lock()
	rec, recOK := roomRecorders.m[id]
	if recOK {
		delete(roomRecorders.m, id)
	}
	roomRecorders.Unlock()
	if !recOK {
		return "", nil, false
	}

	roomRecordStorage.Lock()
	backend := roomRecordStorage.m[id]
	delete(roomRecordStorage.m, id)
	roomRecordStorage.Unlock()

	fpath := rec.Stop()
	rec.Wait()

	location = fpath
	if backend != nil {
		loc, err := backend.Upload(context.Background(), fpath)
		if err != nil {
			s.Log.Error("storage upload failed", "room_id", id, "error", err)
		} else {
			location = loc
		}
	}

	return location, mcResult, true
}

func (s *Server) stopRecordRoom(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	location, mcResult, ok := s.cleanupRoomRecording(id)
	if !ok {
		writeError(w, http.StatusNotFound, "no recording in progress")
		return
	}

	roomAppID := ""
	if rm, ok := s.RoomMgr.Get(id); ok {
		roomAppID = rm.AppID
	}
	resp := map[string]interface{}{"status": "stopped", "file": location}
	evtData := &events.RecordingFinishedData{
		LegRoomScope: events.LegRoomScope{RoomID: id, AppID: roomAppID},
		File:         location,
	}
	if mcResult != nil {
		resp["multi_channel_file"] = mcResult.FilePath
		channels := make(map[string]interface{}, len(mcResult.Channels))
		for legID, ch := range mcResult.Channels {
			channels[legID] = map[string]interface{}{
				"channel":  ch.Channel,
				"start_ms": ch.StartMs,
				"end_ms":   ch.EndMs,
			}
		}
		resp["channels"] = channels
		evtData.MultiChannelFile = mcResult.FilePath
		evtData.Channels = mcResult.Channels
	}

	s.Bus.Publish(events.RecordingFinished, evtData)
	writeJSON(w, http.StatusOK, resp)
}

// setRoomRecordingPaused applies paused to the room mix recorder and, if
// multi-channel recording is active, to every per-participant recorder as
// well. Returns true if any recorder's state actually changed.
func setRoomRecordingPaused(roomID string, paused bool) (changed bool, found bool) {
	roomRecorders.Lock()
	rec, ok := roomRecorders.m[roomID]
	roomRecorders.Unlock()
	if !ok {
		return false, false
	}
	if paused {
		changed = rec.Pause()
	} else {
		changed = rec.Resume()
	}

	roomMultiChannel.Lock()
	mc := roomMultiChannel.m[roomID]
	roomMultiChannel.Unlock()
	if mc != nil {
		mc.mu.Lock()
		mc.paused = paused
		recs := make([]*recording.Recorder, 0, len(mc.recorders))
		for _, r := range mc.recorders {
			recs = append(recs, r)
		}
		mc.mu.Unlock()
		for _, r := range recs {
			var c bool
			if paused {
				c = r.Pause()
			} else {
				c = r.Resume()
			}
			changed = changed || c
		}
	}
	return changed, true
}

func (s *Server) pauseRecordRoom(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	changed, ok := setRoomRecordingPaused(id, true)
	if !ok {
		writeError(w, http.StatusNotFound, "no recording in progress")
		return
	}
	if !changed {
		writeJSON(w, http.StatusOK, map[string]interface{}{"status": "already_paused"})
		return
	}
	roomAppID := ""
	if rm, ok := s.RoomMgr.Get(id); ok {
		roomAppID = rm.AppID
	}
	s.Bus.Publish(events.RecordingPaused, &events.RecordingPausedData{
		LegRoomScope: events.LegRoomScope{RoomID: id, AppID: roomAppID},
	})
	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "paused"})
}

func (s *Server) resumeRecordRoom(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	changed, ok := setRoomRecordingPaused(id, false)
	if !ok {
		writeError(w, http.StatusNotFound, "no recording in progress")
		return
	}
	if !changed {
		writeJSON(w, http.StatusOK, map[string]interface{}{"status": "not_paused"})
		return
	}
	roomAppID := ""
	if rm, ok := s.RoomMgr.Get(id); ok {
		roomAppID = rm.AppID
	}
	s.Bus.Publish(events.RecordingResumed, &events.RecordingResumedData{
		LegRoomScope: events.LegRoomScope{RoomID: id, AppID: roomAppID},
	})
	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "resumed"})
}

// stopRoomRecordingIfEmpty stops the room's recording when no leg participants
// remain. Called after a leg is removed from a room.
func (s *Server) stopRoomRecordingIfEmpty(roomID string) {
	rm, ok := s.RoomMgr.Get(roomID)
	if !ok || rm.ParticipantCount() > 0 {
		return
	}

	location, mcResult, ok := s.cleanupRoomRecording(roomID)
	if !ok {
		return
	}

	roomAppID := ""
	if rm, ok := s.RoomMgr.Get(roomID); ok {
		roomAppID = rm.AppID
	}
	evtData := &events.RecordingFinishedData{
		LegRoomScope: events.LegRoomScope{RoomID: roomID, AppID: roomAppID},
		File:         location,
	}
	if mcResult != nil {
		evtData.MultiChannelFile = mcResult.FilePath
		evtData.Channels = mcResult.Channels
	}
	s.Bus.Publish(events.RecordingFinished, evtData)
	s.Log.Info("auto-stopped room recording (empty room)", "room_id", roomID, "file", location)
}

// onLegJoinedRoomRecording starts a per-participant recording if multi-channel
// recording is active for the room. Called from onLegJoinedRoom.
func (s *Server) onLegJoinedRoomRecording(roomID, legID string) {
	roomMultiChannel.Lock()
	mc := roomMultiChannel.m[roomID]
	roomMultiChannel.Unlock()
	if mc == nil {
		return
	}

	rm, ok := s.RoomMgr.Get(roomID)
	if !ok {
		return
	}
	mc.startLeg(legID, rm.Mixer(), s.Config.RecordingDir)
}

// onLegLeavingRoomRecording stops the per-participant recording for a leg
// that is leaving a room with active multi-channel recording.
func (s *Server) onLegLeavingRoomRecording(roomID, legID string) {
	roomMultiChannel.Lock()
	mc := roomMultiChannel.m[roomID]
	roomMultiChannel.Unlock()
	if mc == nil {
		return
	}

	rm, ok := s.RoomMgr.Get(roomID)
	if !ok {
		return
	}
	mc.stopLeg(legID, rm.Mixer())
}

func createPipe() (*pipeReader, *pipeWriter) {
	ch := make(chan []byte, 100)
	done := make(chan struct{})
	return &pipeReader{ch: ch, done: done}, &pipeWriter{ch: ch, done: done}
}

type pipeReader struct {
	ch   chan []byte
	done chan struct{}
	buf  []byte
}

func (r *pipeReader) Read(p []byte) (int, error) {
	if len(r.buf) > 0 {
		n := copy(p, r.buf)
		r.buf = r.buf[n:]
		return n, nil
	}
	select {
	case data := <-r.ch:
		n := copy(p, data)
		if n < len(data) {
			r.buf = data[n:]
		}
		return n, nil
	case <-r.done:
		return 0, io.EOF
	}
}

type pipeWriter struct {
	ch     chan []byte
	done   chan struct{}
	closed atomic.Bool
}

func (w *pipeWriter) Write(p []byte) (int, error) {
	if w.closed.Load() {
		return len(p), nil
	}
	data := make([]byte, len(p))
	copy(data, p)
	select {
	case w.ch <- data:
	case <-w.done:
	default:
		// Drop if buffer full
	}
	return len(p), nil
}

// Close signals the reader to return io.EOF and stops accepting writes.
func (w *pipeWriter) Close() {
	if w.closed.CompareAndSwap(false, true) {
		close(w.done)
	}
}
