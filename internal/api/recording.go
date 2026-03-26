package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/csiwek/VoiceBlender/internal/events"
	"github.com/csiwek/VoiceBlender/internal/leg"
	"github.com/csiwek/VoiceBlender/internal/mixer"
	"github.com/csiwek/VoiceBlender/internal/recording"
	"github.com/csiwek/VoiceBlender/internal/storage"
	"github.com/go-chi/chi/v5"
)

type recordRequest struct {
	Storage     string `json:"storage"`
	S3Bucket    string `json:"s3_bucket"`
	S3Region    string `json:"s3_region"`
	S3Endpoint  string `json:"s3_endpoint"`
	S3Prefix    string `json:"s3_prefix"`
	S3AccessKey string `json:"s3_access_key"`
	S3SecretKey string `json:"s3_secret_key"`
}

// legRecordInfo tracks state needed to cleanly stop a stereo leg recording.
type legRecordInfo struct {
	roomID  string
	pipes   []*pipeWriter
	storage storage.Backend
}

var (
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
func (s *Server) resolveStorage(req recordRequest) (storage.Backend, error) {
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

	var req recordRequest
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

		fpath, recErr = rec.StartStereo(l.Context(), leftPR, rightPR, s.Config.RecordingDir, uint32(mixer.SampleRate))
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

	s.Bus.Publish(events.RecordingStarted, map[string]interface{}{"leg_id": id, "file": fpath})
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

	s.Bus.Publish(events.RecordingFinished, map[string]interface{}{"leg_id": legID, "file": location})
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

func (s *Server) recordRoom(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	rm, ok := s.RoomMgr.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "room not found")
		return
	}

	var req recordRequest
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

	// Use the mixer tap via a pipe for room recording
	pr, pw := createPipe()
	rm.Mixer().SetTap(pw)

	rec := recording.NewRecorder(s.Log)
	// Room recordings come from the mixer tap which runs at 16kHz
	fpath, err := rec.StartAt(parts[0].Context(), pr, s.Config.RecordingDir, uint32(mixer.SampleRate))
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

	s.Bus.Publish(events.RecordingStarted, map[string]interface{}{"room_id": id, "file": fpath})
	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "recording", "file": fpath})
}

func (s *Server) stopRecordRoom(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	rm, ok := s.RoomMgr.Get(id)
	if ok {
		rm.Mixer().SetTap(nil)
	}

	roomRecordPipes.Lock()
	pw := roomRecordPipes.m[id]
	delete(roomRecordPipes.m, id)
	roomRecordPipes.Unlock()
	if pw != nil {
		pw.Close()
	}

	roomRecorders.Lock()
	rec, ok := roomRecorders.m[id]
	if ok {
		delete(roomRecorders.m, id)
	}
	roomRecorders.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "no recording in progress")
		return
	}

	roomRecordStorage.Lock()
	backend := roomRecordStorage.m[id]
	delete(roomRecordStorage.m, id)
	roomRecordStorage.Unlock()

	fpath := rec.Stop()
	rec.Wait()

	location := fpath
	if backend != nil {
		loc, err := backend.Upload(context.Background(), fpath)
		if err != nil {
			s.Log.Error("storage upload failed", "room_id", id, "error", err)
		} else {
			location = loc
		}
	}

	s.Bus.Publish(events.RecordingFinished, map[string]interface{}{"room_id": id, "file": location})
	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "stopped", "file": location})
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
