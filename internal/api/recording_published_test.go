package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/recording"
)

// discardedRecorder returns a recorder whose capture failed and was therefore
// discarded, along with the path Stop will nonetheless report. A stereo
// companion that cannot be drained without blocking is a real, reachable
// capture error, so the staging file never reaches that path.
func discardedRecorder(t *testing.T, dir string) (*recording.Recorder, string) {
	t.Helper()
	rec := recording.NewRecorder(slog.Default())
	fpath, err := rec.StartStereo(context.Background(), bytes.NewReader(nil), bytes.NewReader(nil), dir, 8000)
	if err != nil {
		t.Fatalf("start discarded recorder: %v", err)
	}
	rec.Wait()
	if rec.Published() {
		t.Fatal("precondition: the capture published, so this test proves nothing")
	}
	if _, err := os.Stat(fpath); !os.IsNotExist(err) {
		t.Fatalf("precondition: something exists at %s, so this test proves nothing", fpath)
	}
	return rec, fpath
}

// newMultiChannelState builds an active state with every map initialised, the
// shape startLeg and stopAll both assume.
func newMultiChannelState(t *testing.T, dir string, startTime time.Time) *multiChannelState {
	t.Helper()
	return &multiChannelState{
		active:       true,
		startTime:    startTime,
		sampleRate:   8000,
		dir:          dir,
		recorders:    map[string]*recording.Recorder{},
		pipes:        map[string]*pipeWriter{},
		files:        map[string]string{},
		joinOffsets:  map[string]time.Duration{},
		leaveOffsets: map[string]time.Duration{},
		log:          slog.Default(),
	}
}

// unstartableDir returns a recording directory the recorder cannot create: its
// parent is a regular file, so MkdirAll fails with ENOTDIR. A merely absent
// directory is no good here — the recorder creates it, and as root it creates
// it almost anywhere.
func unstartableDir(t *testing.T) string {
	t.Helper()
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatalf("write blocker file: %v", err)
	}
	return filepath.Join(blocker, "recordings")
}

// TestStopLegRecording_DiscardedCaptureReportsNoFile pins the leg stop path.
// Stop reports the path the capture was headed for whether or not it got there,
// so returning it unconditionally handed the caller a 200 naming a file that is
// not on disk — and, with a storage backend configured, sent that nonexistent
// path to Upload.
func TestStopLegRecording_DiscardedCaptureReportsNoFile(t *testing.T) {
	s := newTestServer(t)
	const legID = "discarded-leg"

	rec, fpath := discardedRecorder(t, t.TempDir())
	legRecorders.Lock()
	legRecorders.m[legID] = rec
	legRecorders.Unlock()
	t.Cleanup(func() {
		legRecorders.Lock()
		delete(legRecorders.m, legID)
		legRecorders.Unlock()
	})

	var got *events.RecordingFinishedData
	unsub := s.Bus.Subscribe(func(e events.Event) {
		if e.Type != events.RecordingFinished {
			return
		}
		if d, ok := e.Data.(*events.RecordingFinishedData); ok {
			got = d
		}
	})
	defer unsub()

	loc, ok := s.stopLegRecording(legID)
	if !ok {
		t.Fatal("stopLegRecording reported no recording, though one was in progress")
	}
	if loc != "" {
		t.Errorf("stopLegRecording returned %q, want no location — nothing was written to %s", loc, fpath)
	}
	if got == nil {
		t.Fatal("no recording.finished was published, so this test proves nothing")
	}
	if got.File == fpath {
		t.Errorf("recording.finished carries File = %q, but no file exists there", got.File)
	}
	// The event type has no omitempty on File, so unlike the REST result it
	// carries an empty string rather than dropping the key. API.md documents
	// exactly this; pin it so the two cannot drift apart silently.
	body, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	if decoded["file"] != "" {
		t.Errorf("recording.finished file = %v, want an empty string; API.md documents that shape", decoded["file"])
	}
}

// TestStopLegResult_OmitsFileWhenNothingPublished is the wire-level half: a
// discarded capture must not put a `file` key in the response at all, which is
// what the regenerated spec now says by leaving it out of `required`.
func TestStopLegResult_OmitsFileWhenNothingPublished(t *testing.T) {
	s := newTestServer(t)
	const legID = "discarded-leg-wire"

	rec, _ := discardedRecorder(t, t.TempDir())
	legRecorders.Lock()
	legRecorders.m[legID] = rec
	legRecorders.Unlock()
	t.Cleanup(func() {
		legRecorders.Lock()
		delete(legRecorders.m, legID)
		legRecorders.Unlock()
	})

	res, err := s.doStopRecordLeg(legID)
	if err != nil {
		t.Fatalf("doStopRecordLeg: %v", err)
	}
	if res.Status != "stopped" {
		t.Errorf("Status = %q, want stopped — the stop itself succeeded", res.Status)
	}
	body, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if _, present := decoded["file"]; present {
		t.Errorf("response carries a file key though nothing was published: %s", body)
	}
}

// TestCleanupRoomRecording_DiscardedMixReportsNoFile pins the room stop path,
// which had the same unconditional return as the leg path. The stop must still
// report that a recording was in progress: answering "no recording" would turn
// the API stop into a 404 and throw away any multi-channel result with it.
func TestCleanupRoomRecording_DiscardedMixReportsNoFile(t *testing.T) {
	s := newTestServer(t)
	const roomID = "discarded-mix-room"
	if _, err := s.RoomMgr.Create(roomID, "test-app", 8000); err != nil {
		t.Fatalf("create room: %v", err)
	}

	rec, fpath := discardedRecorder(t, t.TempDir())
	roomRecorders.Lock()
	roomRecorders.m[roomID] = rec
	roomRecorders.Unlock()
	t.Cleanup(func() {
		roomRecorders.Lock()
		delete(roomRecorders.m, roomID)
		roomRecorders.Unlock()
	})

	location, _, ok := s.cleanupRoomRecording(roomID)
	if !ok {
		t.Fatal("cleanupRoomRecording reported no recording, though one was in progress")
	}
	if location != "" {
		t.Errorf("cleanupRoomRecording returned %q, want no location — nothing was written to %s", location, fpath)
	}
}

// TestMultiChannelStartLeg_FailedStartIsReportedOmitted covers the leg that
// never started. It enters neither recorders nor files, so participantOrder is
// the only place stopAll's walk can find it — without it the leg was absent
// from the merge and from omitted_legs alike, and the operator was told the
// recording was complete.
func TestMultiChannelStartLeg_FailedStartIsReportedOmitted(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	// Backdated so the merge spans a real duration and actually writes frames.
	mc := newMultiChannelState(t, dir, time.Now().Add(-time.Second))

	// A healthy leg, so the merge has something to produce and the omission is
	// reported on a successful stop rather than swallowed by a failing one.
	good := recording.NewRecorder(slog.Default())
	if _, err := good.StartAt(ctx, bytes.NewReader(make([]byte, 16000)), dir, 8000); err != nil {
		t.Fatalf("start good leg: %v", err)
	}
	good.Wait()
	if !good.Published() {
		t.Fatal("precondition: the good leg did not publish, so this test proves nothing")
	}
	mc.recorders["good"] = good
	mc.participantOrder = []string{"good"}

	// An uncreatable recording directory is a real, reachable per-leg start failure.
	mc.startLeg("bad", fakeMixer{}, unstartableDir(t))
	if _, started := mc.recorders["bad"]; started {
		t.Fatal("precondition: the leg started, so this test proves nothing")
	}

	res, err := mc.stopAll(fakeMixer{})
	if err != nil {
		t.Fatalf("stopAll: %v", err)
	}
	if _, ok := res.Channels["bad"]; ok {
		t.Errorf("the leg that never started was given a channel; channels = %v", res.Channels)
	}
	if names := res.OmittedLegs; len(names) != 1 || names[0] != "bad" {
		t.Errorf("OmittedLegs = %v, want [bad] — a leg that never started must not vanish", names)
	}
}

// TestMultiChannelStartLeg_ParticipantRecordedAtMostOnce walks every ordering
// that reaches the participantOrder append. A failed start is recorded there so
// stopAll can report it omitted, but a failed start never enters recorders — so
// the recorders guard cannot catch a retry, and each retried ordering could
// give one leg two channel positions. That duplicates the leg's channel in the
// merge (numChannels is len(inputs)) and leaves Channels, keyed by leg ID,
// naming only the later index.
func TestMultiChannelStartLeg_ParticipantRecordedAtMostOnce(t *testing.T) {
	tests := []struct {
		name  string
		dirs  []func(t *testing.T) string // one per startLeg call, in order
		joins bool                        // whether a successful start is expected
	}{
		{
			name:  "start succeeds",
			dirs:  []func(*testing.T) string{func(t *testing.T) string { return t.TempDir() }},
			joins: true,
		},
		{
			name: "start fails then fails again",
			dirs: []func(*testing.T) string{unstartableDir, unstartableDir},
		},
		{
			name: "start fails then succeeds on retry",
			dirs: []func(*testing.T) string{
				unstartableDir,
				func(t *testing.T) string { return t.TempDir() },
			},
			joins: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mc := newMultiChannelState(t, t.TempDir(), time.Now())
			for _, dir := range tc.dirs {
				mc.startLeg("flaky", fakeMixer{}, dir(t))
			}

			if got := mc.participantOrder; len(got) != 1 || got[0] != "flaky" {
				t.Fatalf("participantOrder = %v, want [flaky] exactly once — a duplicate "+
					"position duplicates the leg's channel in the merge", got)
			}
			if _, started := mc.recorders["flaky"]; started != tc.joins {
				t.Errorf("recorders has flaky = %v, want %v", started, tc.joins)
			}
		})
	}
}
