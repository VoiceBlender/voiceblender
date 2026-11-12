package api

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/recording"
)

// gatedReader yields one frame and then blocks in Read until released. A capture
// reading from it cannot finish, so the Stop and Wait that end it stay in flight
// for as long as the test needs.
type gatedReader struct {
	sent    bool
	once    sync.Once
	blocked chan struct{}
	release chan struct{}
}

func newGatedReader() *gatedReader {
	return &gatedReader{blocked: make(chan struct{}), release: make(chan struct{})}
}

func (g *gatedReader) Read(p []byte) (int, error) {
	if !g.sent {
		g.sent = true
		clear(p)
		return len(p), nil
	}
	g.once.Do(func() { close(g.blocked) })
	<-g.release
	return 0, io.EOF
}

// startGatedLeg starts a per-leg capture that has written audio and is now
// stuck in its source, and returns the recorder and the gate that frees it.
func startGatedLeg(t *testing.T, dir string) (*recording.Recorder, *gatedReader) {
	t.Helper()
	src := newGatedReader()
	rec := recording.NewRecorder(slog.Default())
	if _, err := rec.StartAt(context.Background(), src, dir, 8000, ""); err != nil {
		t.Fatalf("start leg capture: %v", err)
	}
	select {
	case <-src.blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("the leg capture never read its source")
	}
	return rec, src
}

func newTestMultiChannelState(dir string) *multiChannelState {
	return &multiChannelState{
		active:       true,
		startTime:    time.Now().Add(-time.Second),
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

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestMultiChannelStopAll_WaitsForLegStopInFlight forces the order a hang-up
// followed at once by a room delete produces: the leg's own stop has taken its
// recorder and is still finalising the file when the room's stop runs. The leg
// is then in neither the recorders nor the files, and a stop that does not wait
// for it merges without it.
func TestMultiChannelStopAll_WaitsForLegStopInFlight(t *testing.T) {
	dir := t.TempDir()
	mc := newTestMultiChannelState(dir)

	rec, src := startGatedLeg(t, dir)
	mc.recorders["a"] = rec
	mc.participantOrder = []string{"a"}

	legStopped := make(chan struct{})
	go func() {
		defer close(legStopped)
		mc.stopLeg("a", fakeMixer{})
	}()
	waitUntil(t, "the leg's stop has claimed its recorder", func() bool {
		mc.mu.Lock()
		defer mc.mu.Unlock()
		_, recording := mc.recorders["a"]
		return !recording
	})

	type stopResult struct {
		res *recording.MultiChannelResult
		err error
	}
	stopped := make(chan stopResult, 1)
	go func() {
		res, err := mc.stopAll(fakeMixer{})
		stopped <- stopResult{res, err}
	}()

	select {
	case r := <-stopped:
		close(src.release)
		<-legStopped
		t.Fatalf("stopAll returned while a leg's capture was still being finalised: result = %+v, err = %v", r.res, r.err)
	case <-time.After(100 * time.Millisecond):
	}

	close(src.release)
	<-legStopped
	r := <-stopped
	if r.err != nil {
		t.Fatalf("stopAll: %v", r.err)
	}
	if _, ok := r.res.Channels["a"]; !ok {
		t.Errorf("the leg has no channel; channels = %v", r.res.Channels)
	}
	if len(r.res.OmittedLegs) != 0 {
		t.Errorf("OmittedLegs = %v, want none: the capture was finalised, not lost", r.res.OmittedLegs)
	}
}

// TestRoomRecordingStop_RacingStopsReportOnce covers two stops of one room
// recording at the same time, which is what a room delete and the last leg
// leaving are. The stop that arrives second must not take the mix recorder from
// under the first and publish recording.finished without the multi-channel
// result, leaving the first with a merged file and nothing to report it on.
func TestRoomRecordingStop_RacingStopsReportOnce(t *testing.T) {
	dir := t.TempDir()
	s := newTestServer(t)

	const roomID = "racing-stops-room"
	if _, err := s.RoomMgr.Create(roomID, "test-app", 8000); err != nil {
		t.Fatalf("create room: %v", err)
	}

	mix := recording.NewRecorder(slog.Default())
	if _, err := mix.StartAt(context.Background(), bytes.NewReader(make([]byte, 16000)), dir, 8000, ""); err != nil {
		t.Fatalf("start room mix recorder: %v", err)
	}
	roomRecorders.Lock()
	roomRecorders.m[roomID] = mix
	roomRecorders.Unlock()
	t.Cleanup(func() {
		roomRecorders.Lock()
		delete(roomRecorders.m, roomID)
		roomRecorders.Unlock()
	})

	rec, src := startGatedLeg(t, dir)
	mc := newTestMultiChannelState(dir)
	mc.recorders["a"] = rec
	mc.participantOrder = []string{"a"}
	roomMultiChannel.Lock()
	roomMultiChannel.m[roomID] = mc
	roomMultiChannel.Unlock()
	t.Cleanup(func() {
		roomMultiChannel.Lock()
		delete(roomMultiChannel.m, roomID)
		roomMultiChannel.Unlock()
	})

	var mu sync.Mutex
	var finished []*events.RecordingFinishedData
	unsub := s.Bus.Subscribe(func(e events.Event) {
		if d, ok := e.Data.(*events.RecordingFinishedData); ok && e.Type == events.RecordingFinished {
			mu.Lock()
			finished = append(finished, d)
			mu.Unlock()
		}
	})
	defer unsub()

	// The first stop takes the multi-channel state and then waits on the leg.
	firstDone := make(chan bool, 1)
	go func() { firstDone <- s.finalizeRoomRecording(roomID, "test-app", "empty room") }()
	waitUntil(t, "the first stop has taken the multi-channel state", func() bool {
		roomMultiChannel.Lock()
		defer roomMultiChannel.Unlock()
		return roomMultiChannel.m[roomID] == nil
	})

	secondDone := make(chan bool, 1)
	go func() { secondDone <- s.finalizeRoomRecording(roomID, "test-app", "room deleted") }()
	var secondOK, secondReturned bool
	select {
	case secondOK = <-secondDone:
		secondReturned = true
	case <-time.After(100 * time.Millisecond):
	}

	close(src.release)
	firstOK := <-firstDone
	if !secondReturned {
		secondOK = <-secondDone
	}

	if !firstOK {
		t.Error("the stop that owned the recording reported nothing to stop")
	}
	if secondOK {
		t.Error("the second stop also reported a recording stopped")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(finished) != 1 {
		t.Fatalf("recording.finished published %d times, want 1", len(finished))
	}
	if finished[0].MultiChannelFile == "" {
		t.Error("recording.finished has no multi_channel_file: the merge result was dropped")
	}
	if _, ok := finished[0].Channels["a"]; !ok {
		t.Errorf("the leg has no channel in the event; channels = %v", finished[0].Channels)
	}
}
