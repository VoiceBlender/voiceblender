package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/VoiceBlender/voiceblender/internal/mixer"
	"github.com/VoiceBlender/voiceblender/internal/recording"
	"github.com/VoiceBlender/voiceblender/internal/storage"
)

// siprecCapture is one recorded party's mono WAV, tapped off the leg stream
// carrying their audio.
type siprecCapture struct {
	streamID string
	// channel is the key this party appears under in the merged file's channel
	// map: their AOR, display name or participant ID, falling back to the SDP
	// label when the metadata has bound nothing to the stream.
	channel string
	rec     *recording.Recorder
	pipe    *pipeWriter
	file    string
}

// siprecRecording captures every stream of a recording session separately and
// merges them into one multi-channel WAV, one channel per recorded party.
//
// A leg-level stereo recording is the wrong shape here: a SIPREC leg's audio is
// N receive-only streams belonging to N different people, not one call's in and
// out. The mixer's per-participant machinery is also wrong, because these
// streams need not be in any room.
type siprecRecording struct {
	mu sync.Mutex
	// sampleRate is what every capture is resampled to. Streams on one session
	// can negotiate different codecs, and MergeMultiChannel takes a single rate
	// for all its inputs.
	sampleRate int
	startTime  time.Time
	dir        string
	storage    storage.Backend
	captures   []*siprecCapture
	log        *slog.Logger
}

// startSIPRECRecording taps every stream of the session and starts one mono
// recorder per party. It returns the recording, or an error from the first
// capture that could not start.
func (s *Server) startSIPRECRecording(sl *leg.SIPLeg, sess *siprecSession, backend storage.Backend) (*siprecRecording, error) {
	rate := s.Config.DefaultSampleRate
	if rate <= 0 {
		rate = 16000
	}

	sr := &siprecRecording{
		sampleRate: rate,
		startTime:  time.Now(),
		dir:        s.Config.RecordingDir,
		storage:    backend,
		log:        s.Log,
	}

	used := make(map[string]int)
	for _, info := range sl.Streams() {
		channel := siprecChannelName(sess, info, used)

		pr, pw := createPipe()
		streamRate := info.SampleRate
		if streamRate <= 0 {
			streamRate = rate
		}
		if !sl.SetStreamInTap(info.ID, mixer.NewResampleWriter(pw, streamRate, rate)) {
			pw.Close()
			continue
		}

		rec := recording.NewRecorder(s.Log)
		if _, err := rec.StartAt(sl.Context(), pr, sr.dir, uint32(rate), ""); err != nil {
			sl.ClearStreamInTap(info.ID)
			pw.Close()
			sr.stopAll(sl)
			return nil, err
		}
		sr.captures = append(sr.captures, &siprecCapture{
			streamID: info.ID,
			channel:  channel,
			rec:      rec,
			pipe:     pw,
		})
	}

	if len(sr.captures) == 0 {
		return nil, errNoSIPRECStreams
	}
	return sr, nil
}

// siprecChannelName picks the key a party's audio appears under. Identity beats
// the SDP label, but the label is what disambiguates two parties the metadata
// names identically — a merged file with a collapsed channel map would silently
// lose one of them.
func siprecChannelName(sess *siprecSession, info leg.StreamInfo, used map[string]int) string {
	name := "label:" + info.Label
	if info.Label == "" {
		name = "stream:" + info.ID
	}
	if p, ok := sess.state.ParticipantForLabel(info.Label); ok {
		name = p.Label()
	}

	used[name]++
	if n := used[name]; n > 1 {
		if info.Label != "" {
			return name + " (label:" + info.Label + ")"
		}
		return name + " (stream:" + info.ID + ")"
	}
	return name
}

// stopAll closes every capture and returns the merged multi-channel result.
// Captures that produced nothing are reported as omitted rather than failing
// the merge, which would destroy every other party's audio too.
func (sr *siprecRecording) stopAll(sl *leg.SIPLeg) (*recording.MultiChannelResult, error) {
	sr.mu.Lock()
	defer sr.mu.Unlock()

	total := time.Since(sr.startTime)

	for _, c := range sr.captures {
		if sl != nil {
			sl.ClearStreamInTap(c.streamID)
		}
		if c.pipe != nil {
			c.pipe.Close()
		}
		fpath := c.rec.Stop()
		c.rec.Wait()
		if c.rec.Finalized() {
			c.file = fpath
			continue
		}
		sr.log.Error("SIPREC: stream capture was discarded, dropping it from the merge",
			"stream_id", c.streamID, "channel", c.channel, "file", fpath)
	}

	var (
		inputs  []recording.MultiChannelInput
		omitted []string
	)
	for _, c := range sr.captures {
		if c.file == "" {
			omitted = append(omitted, c.channel)
			continue
		}
		inputs = append(inputs, recording.MultiChannelInput{
			LegID:    c.channel,
			FilePath: c.file,
		})
	}
	if len(inputs) == 0 {
		return &recording.MultiChannelResult{OmittedLegs: omitted}, nil
	}

	res, err := recording.MergeMultiChannel(sr.dir, inputs, total, sr.sampleRate)
	if err != nil {
		return nil, err
	}
	res.OmittedLegs = omitted
	return res, nil
}

// pause and resume apply to every capture, so the channels stay time-aligned.
func (sr *siprecRecording) setPaused(paused bool) {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	for _, c := range sr.captures {
		if paused {
			c.rec.Pause()
		} else {
			c.rec.Resume()
		}
	}
}

func (sr *siprecRecording) isPaused() bool {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	return len(sr.captures) > 0 && sr.captures[0].rec.IsPaused()
}

// errNoSIPRECStreams is returned when a session has no stream that can be
// tapped, so there is nothing to record.
var errNoSIPRECStreams = errors.New("recording session has no capturable streams")

// startSIPRECLegRecording is the /record entry point for a recording session.
func (s *Server) startSIPRECLegRecording(l leg.Leg, backend storage.Backend) (*RecordingStartResult, error) {
	sl, ok := l.(*leg.SIPLeg)
	if !ok {
		return nil, newAPIError(http.StatusConflict, "recording session leg is not a SIP leg")
	}
	sess, ok := s.siprecSession(l.ID())
	if !ok {
		return nil, newAPIError(http.StatusNotFound, "recording session state not found")
	}

	s.siprecMu.Lock()
	_, already := s.siprecRecordings[l.ID()]
	s.siprecMu.Unlock()
	if already {
		return nil, newAPIError(http.StatusConflict, "recording session is already being recorded")
	}

	sr, err := s.startSIPRECRecording(sl, sess, backend)
	if err != nil {
		if errors.Is(err, errNoSIPRECStreams) {
			return nil, newAPIError(http.StatusConflict, "%s", err.Error())
		}
		return nil, recordingStartAPIError(err)
	}

	s.siprecMu.Lock()
	s.siprecRecordings[l.ID()] = sr
	s.siprecMu.Unlock()

	// The merged file does not exist until the recording stops, so there is no
	// path to advertise yet — unlike a single-file capture.
	s.Bus.Publish(events.RecordingStarted, &events.RecordingStartedData{
		LegRoomScope: events.LegRoomScope{LegID: l.ID(), AppID: l.AppID()},
	})
	return &RecordingStartResult{Status: "recording"}, nil
}

// stopSIPRECLegRecording stops and merges a recording session's capture. The
// bool reports whether this leg had one, so the ordinary stop path can carry on
// when it did not.
func (s *Server) stopSIPRECLegRecording(legID string) (string, bool) {
	s.siprecMu.Lock()
	sr, ok := s.siprecRecordings[legID]
	delete(s.siprecRecordings, legID)
	s.siprecMu.Unlock()
	if !ok {
		return "", false
	}

	var sl *leg.SIPLeg
	appID := ""
	if l, lOK := s.LegMgr.Get(legID); lOK {
		appID = l.AppID()
		sl, _ = l.(*leg.SIPLeg)
	}

	defer s.releaseBackend(sr.storage)

	res, err := sr.stopAll(sl)
	if err != nil {
		s.Log.Error("SIPREC: merge multi-channel recording failed", "leg_id", legID, "error", err)
		s.Bus.Publish(events.RecordingFinished, &events.RecordingFinishedData{
			LegRoomScope: events.LegRoomScope{LegID: legID, AppID: appID},
		})
		return "", true
	}

	location := sr.upload(context.Background(), res.FilePath)
	s.Bus.Publish(events.RecordingFinished, &events.RecordingFinishedData{
		LegRoomScope:     events.LegRoomScope{LegID: legID, AppID: appID},
		File:             location,
		MultiChannelFile: location,
		Channels:         res.Channels,
		OmittedLegs:      res.OmittedLegs,
	})
	return location, true
}

// setSIPRECRecordingPaused is the pause/resume hook for a recording session.
// The bool reports whether this leg had a session capture.
func (s *Server) setSIPRECRecordingPaused(legID string, paused bool) (string, bool) {
	s.siprecMu.Lock()
	sr, ok := s.siprecRecordings[legID]
	s.siprecMu.Unlock()
	if !ok {
		return "", false
	}
	if sr.isPaused() == paused {
		if paused {
			return "already_paused", true
		}
		return "not_paused", true
	}
	sr.setPaused(paused)
	if paused {
		return "paused", true
	}
	return "resumed", true
}

// maybeAutoRecordSIPREC starts recording a session on arrival when the operator
// asked for it, using the server-wide storage default.
func (s *Server) maybeAutoRecordSIPREC(l leg.Leg) {
	if !s.Config.SIPRECAutoRecord {
		return
	}
	backend, err := s.resolveStorage(context.Background(), RecordRequest{})
	if err != nil {
		s.Log.Error("SIPREC: auto-record storage setup failed", "leg_id", l.ID(), "error", err)
		return
	}
	if _, err := s.startSIPRECLegRecording(l, backend); err != nil {
		s.releaseBackend(backend)
		s.Log.Error("SIPREC: auto-record failed to start", "leg_id", l.ID(), "error", err)
	}
}

// publishRecordingPaused emits the pause/resume event for a leg recording.
func (s *Server) publishRecordingPaused(legID string, paused bool) {
	appID := ""
	if l, ok := s.LegMgr.Get(legID); ok {
		appID = l.AppID()
	}
	scope := events.LegRoomScope{LegID: legID, AppID: appID}
	if paused {
		s.Bus.Publish(events.RecordingPaused, &events.RecordingPausedData{LegRoomScope: scope})
		return
	}
	s.Bus.Publish(events.RecordingResumed, &events.RecordingResumedData{LegRoomScope: scope})
}

// upload publishes the merged file through the storage backend, keeping the
// local path when the upload fails.
func (sr *siprecRecording) upload(ctx context.Context, path string) string {
	if sr.storage == nil || path == "" {
		return path
	}
	loc, err := sr.storage.Upload(ctx, path)
	if err != nil {
		sr.log.Error("SIPREC: storage upload failed", "file", path, "error", err)
		return path
	}
	return loc
}
