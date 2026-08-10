package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/tts"
)

// preflightLeg is a leg with an audio writer whose context the test controls,
// so leg teardown can be simulated.
type preflightLeg struct {
	*apiMockLeg
	ctx context.Context
}

func (l *preflightLeg) AudioWriter() io.Writer   { return io.Discard }
func (l *preflightLeg) SampleRate() int          { return 16000 }
func (l *preflightLeg) Context() context.Context { return l.ctx }

// gatedTTS is a tts.Provider whose synthesis the test releases explicitly, so
// the window between staging and readiness can be inspected.
type gatedTTS struct {
	release chan struct{}
	audio   []byte
	err     error

	mu      sync.Mutex
	started bool
	ctxErr  error
}

func newGatedTTS(audio []byte) *gatedTTS {
	return &gatedTTS{release: make(chan struct{}), audio: audio}
}

func (g *gatedTTS) Synthesize(ctx context.Context, text string, opts tts.Options) (*tts.Result, error) {
	g.mu.Lock()
	g.started = true
	g.mu.Unlock()

	select {
	case <-g.release:
	case <-ctx.Done():
		g.mu.Lock()
		g.ctxErr = ctx.Err()
		g.mu.Unlock()
		return nil, ctx.Err()
	}
	if g.err != nil {
		return nil, g.err
	}
	return &tts.Result{Audio: io.NopCloser(bytes.NewReader(g.audio)), MimeType: "audio/pcm;rate=16000"}, nil
}

func (g *gatedTTS) synthCanceled() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.ctxErr
}

// newPreflightServer wires a server with a controllable leg and TTS provider,
// and drains the package-level staging registry afterwards.
func newPreflightServer(t *testing.T, provider tts.Provider) (*Server, context.CancelFunc) {
	t.Helper()
	s := newTestServer(t)
	s.TTS = provider
	s.Config.TTSPreflightTTL = 5 * time.Second
	s.Config.TTSPreflightMaxPerLeg = 3
	s.Config.TTSPreflightMaxBytes = 4 << 20

	ctx, cancel := context.WithCancel(context.Background())
	s.LegMgr.Add(&preflightLeg{apiMockLeg: &apiMockLeg{id: "leg-1", createdAt: time.Now()}, ctx: ctx})

	t.Cleanup(func() {
		cancel()
		stagedTTSReg.Lock()
		stagedTTSReg.m = make(map[string]map[string]*stagedTTS)
		stagedTTSReg.Unlock()
	})
	return s, cancel
}

type ttsEventSink struct {
	mu sync.Mutex
	ev []events.Event
}

func (k *ttsEventSink) subscribe(s *Server) {
	s.Bus.Subscribe(func(e events.Event) {
		k.mu.Lock()
		defer k.mu.Unlock()
		k.ev = append(k.ev, e)
	})
}

// await returns the first event of the given type, or nil if none arrives.
func (k *ttsEventSink) await(typ events.EventType) events.EventData {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		k.mu.Lock()
		for _, e := range k.ev {
			if e.Type == typ {
				d := e.Data
				k.mu.Unlock()
				return d
			}
		}
		k.mu.Unlock()
		time.Sleep(2 * time.Millisecond)
	}
	return nil
}

func (k *ttsEventSink) seen(typ events.EventType) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, e := range k.ev {
		if e.Type == typ {
			return true
		}
	}
	return false
}

func stagedCount(legID string) int {
	stagedTTSReg.Lock()
	defer stagedTTSReg.Unlock()
	return len(stagedTTSReg.m[legID])
}

func preflightReq() TTSRequest {
	return TTSRequest{Text: "hello", Voice: "v", APIKey: "k"}
}

// 320 bytes of 16kHz mono PCM16 is 10ms of audio.
var pcm10ms = make([]byte, 320)

func TestPreflight_StageThenCommitPlays(t *testing.T) {
	g := newGatedTTS(pcm10ms)
	s, _ := newPreflightServer(t, g)
	var sink ttsEventSink
	sink.subscribe(s)

	res, err := s.doPreflightLegTTS("leg-1", preflightReq())
	if err != nil {
		t.Fatalf("doPreflightLegTTS: %v", err)
	}
	if res.Status != "staged" {
		t.Fatalf("status = %q, want staged", res.Status)
	}
	// Nothing plays until the app commits.
	if sink.seen(events.TTSStarted) {
		t.Fatal("tts.started fired on preflight; staged audio must not play")
	}

	close(g.release)
	staged, _ := sink.await(events.TTSStaged).(*events.TTSStagedData)
	if staged == nil {
		t.Fatal("no tts.staged event")
	}
	if staged.TTSID != res.TTSID || staged.Bytes != len(pcm10ms) || staged.DurationMs != 10 {
		t.Fatalf("staged = %+v, want %d bytes / 10ms for %s", staged, len(pcm10ms), res.TTSID)
	}

	commit, err := s.doCommitLegTTS("leg-1", res.TTSID)
	if err != nil {
		t.Fatalf("doCommitLegTTS: %v", err)
	}
	if commit.Status != "committed" {
		t.Fatalf("status = %q, want committed", commit.Status)
	}
	if sink.await(events.TTSStarted) == nil {
		t.Fatal("no tts.started after commit")
	}
	if sink.await(events.TTSFinished) == nil {
		t.Fatal("no tts.finished after commit")
	}
	if n := stagedCount("leg-1"); n != 0 {
		t.Errorf("%d entries left staged after commit, want 0", n)
	}
}

// Commit is the latency-critical call; it must not make the app poll for
// readiness. It returns 200 and starts playing when the audio lands.
func TestPreflight_CommitBeforeReady(t *testing.T) {
	// 200ms of audio, so "played nothing" is distinguishable from "played it".
	g := newGatedTTS(make([]byte, 6400))
	s, _ := newPreflightServer(t, g)
	var sink ttsEventSink
	sink.subscribe(s)

	res, err := s.doPreflightLegTTS("leg-1", preflightReq())
	if err != nil {
		t.Fatalf("doPreflightLegTTS: %v", err)
	}
	if _, err := s.doCommitLegTTS("leg-1", res.TTSID); err != nil {
		t.Fatalf("commit before synthesis finished: %v", err)
	}
	if sink.seen(events.TTSStarted) {
		t.Fatal("tts.started fired before synthesis completed")
	}

	close(g.release)
	if sink.await(events.TTSStarted) == nil {
		t.Fatal("playback never started once synthesis landed")
	}
	// A commit that races ahead of synthesis must still play the audio, not an
	// empty buffer: tts.started fires either way, so only played_ms proves it.
	fin, _ := sink.await(events.TTSFinished).(*events.TTSFinishedData)
	if fin == nil {
		t.Fatal("no tts.finished after commit")
	}
	if fin.PlayedMs < 100 {
		t.Fatalf("played_ms = %d for 200ms of staged audio; the commit played an empty buffer", fin.PlayedMs)
	}
	// tts.staged after a commit would tell the app to commit something it
	// already committed.
	if sink.seen(events.TTSStaged) {
		t.Error("tts.staged fired for an already-committed utterance")
	}
}

func TestPreflight_DiscardAbortsSynthesis(t *testing.T) {
	g := newGatedTTS(pcm10ms)
	s, _ := newPreflightServer(t, g)
	var sink ttsEventSink
	sink.subscribe(s)

	res, err := s.doPreflightLegTTS("leg-1", preflightReq())
	if err != nil {
		t.Fatalf("doPreflightLegTTS: %v", err)
	}
	if _, err := s.doDiscardLegTTS("leg-1", res.TTSID); err != nil {
		t.Fatalf("doDiscardLegTTS: %v", err)
	}

	d, _ := sink.await(events.TTSDiscarded).(*events.TTSDiscardedData)
	if d == nil || d.Reason != discardApp {
		t.Fatalf("discarded event = %+v, want reason %q", d, discardApp)
	}
	// Paying for audio nobody will hear is the whole thing preflight avoids.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && g.synthCanceled() == nil {
		time.Sleep(2 * time.Millisecond)
	}
	if g.synthCanceled() == nil {
		t.Error("synthesis context was not cancelled by the discard")
	}
	if sink.seen(events.TTSStaged) {
		t.Error("tts.staged fired for a discarded utterance")
	}
	if n := stagedCount("leg-1"); n != 0 {
		t.Errorf("%d entries left staged after discard, want 0", n)
	}
}

func TestPreflight_ExpiresAfterTTL(t *testing.T) {
	g := newGatedTTS(pcm10ms)
	s, _ := newPreflightServer(t, g)
	s.Config.TTSPreflightTTL = 30 * time.Millisecond
	var sink ttsEventSink
	sink.subscribe(s)

	if _, err := s.doPreflightLegTTS("leg-1", preflightReq()); err != nil {
		t.Fatalf("doPreflightLegTTS: %v", err)
	}
	d, _ := sink.await(events.TTSDiscarded).(*events.TTSDiscardedData)
	if d == nil || d.Reason != discardExpired {
		t.Fatalf("discarded event = %+v, want reason %q", d, discardExpired)
	}
	if n := stagedCount("leg-1"); n != 0 {
		t.Errorf("%d entries left staged after expiry, want 0", n)
	}
}

func TestPreflight_LegTeardownDiscards(t *testing.T) {
	g := newGatedTTS(pcm10ms)
	s, cancelLeg := newPreflightServer(t, g)
	var sink ttsEventSink
	sink.subscribe(s)

	if _, err := s.doPreflightLegTTS("leg-1", preflightReq()); err != nil {
		t.Fatalf("doPreflightLegTTS: %v", err)
	}
	cancelLeg()

	d, _ := sink.await(events.TTSDiscarded).(*events.TTSDiscardedData)
	if d == nil || d.Reason != discardLegGone {
		t.Fatalf("discarded event = %+v, want reason %q", d, discardLegGone)
	}
}

func TestPreflight_SynthesisErrorReportsAndClears(t *testing.T) {
	g := newGatedTTS(nil)
	g.err = errors.New(`azure: status 401: body="invalid key"`)
	s, _ := newPreflightServer(t, g)
	var sink ttsEventSink
	sink.subscribe(s)

	res, err := s.doPreflightLegTTS("leg-1", preflightReq())
	if err != nil {
		t.Fatalf("doPreflightLegTTS: %v", err)
	}
	close(g.release)

	e, _ := sink.await(events.TTSError).(*events.TTSErrorData)
	if e == nil {
		t.Fatal("no tts.error for the failed staged synthesis")
	}
	if e.Category != string(tts.CategoryPermanentAuth) {
		t.Errorf("category = %q, want %q", e.Category, tts.CategoryPermanentAuth)
	}
	if sink.seen(events.TTSStarted) {
		t.Error("tts.started fired despite the synthesis failure")
	}
	if n := stagedCount("leg-1"); n != 0 {
		t.Errorf("%d entries left staged after a failure, want 0", n)
	}
	// The failed id is gone: committing it is a 404, not a hang.
	if _, err := s.doCommitLegTTS("leg-1", res.TTSID); err == nil {
		t.Error("commit of a failed staged utterance succeeded")
	}
}

func TestPreflight_OversizeAudioRejected(t *testing.T) {
	g := newGatedTTS(make([]byte, 2048))
	s, _ := newPreflightServer(t, g)
	s.Config.TTSPreflightMaxBytes = 1024
	var sink ttsEventSink
	sink.subscribe(s)

	if _, err := s.doPreflightLegTTS("leg-1", preflightReq()); err != nil {
		t.Fatalf("doPreflightLegTTS: %v", err)
	}
	close(g.release)

	e, _ := sink.await(events.TTSError).(*events.TTSErrorData)
	if e == nil {
		t.Fatal("no tts.error for oversize staged audio")
	}
	if e.Category != string(tts.CategoryPermanentInput) {
		t.Errorf("category = %q, want %q", e.Category, tts.CategoryPermanentInput)
	}
	if n := stagedCount("leg-1"); n != 0 {
		t.Errorf("%d entries left staged, want 0", n)
	}
}

// Evicting the oldest would silently destroy an utterance the app is about to
// commit, so the cap refuses loudly instead.
func TestPreflight_PerLegCapRefuses(t *testing.T) {
	g := newGatedTTS(pcm10ms)
	s, _ := newPreflightServer(t, g)
	s.Config.TTSPreflightMaxPerLeg = 2

	for i := 0; i < 2; i++ {
		if _, err := s.doPreflightLegTTS("leg-1", preflightReq()); err != nil {
			t.Fatalf("preflight %d: %v", i, err)
		}
	}
	_, err := s.doPreflightLegTTS("leg-1", preflightReq())
	if err == nil {
		t.Fatal("preflight past the cap succeeded")
	}
	if ae, ok := err.(*apiError); !ok || ae.Code != 409 {
		t.Fatalf("error = %v, want a 409", err)
	}
}

func TestPreflight_LifecycleErrors(t *testing.T) {
	g := newGatedTTS(pcm10ms)
	s, _ := newPreflightServer(t, g)
	close(g.release)

	t.Run("commit_unknown_id", func(t *testing.T) {
		_, err := s.doCommitLegTTS("leg-1", "tts-nope")
		if ae, ok := err.(*apiError); !ok || ae.Code != 404 {
			t.Fatalf("error = %v, want a 404", err)
		}
	})

	t.Run("discard_unknown_id", func(t *testing.T) {
		_, err := s.doDiscardLegTTS("leg-1", "tts-nope")
		if ae, ok := err.(*apiError); !ok || ae.Code != 404 {
			t.Fatalf("error = %v, want a 404", err)
		}
	})

	t.Run("double_commit_and_discard_after_commit", func(t *testing.T) {
		res, err := s.doPreflightLegTTS("leg-1", preflightReq())
		if err != nil {
			t.Fatalf("doPreflightLegTTS: %v", err)
		}
		if _, err := s.doCommitLegTTS("leg-1", res.TTSID); err != nil {
			t.Fatalf("first commit: %v", err)
		}
		if _, err := s.doCommitLegTTS("leg-1", res.TTSID); err == nil {
			t.Error("second commit succeeded")
		}
		// While it is still playing, discard must point at the stop verb that
		// actually works rather than pretend to have dropped it.
		if _, err := s.doDiscardLegTTS("leg-1", res.TTSID); err != nil {
			if ae, ok := err.(*apiError); !ok || (ae.Code != 409 && ae.Code != 404) {
				t.Errorf("discard after commit = %v, want 409 or 404", err)
			}
		}
	})
}

// A staged utterance shares the tts id space with playback, so leg_play_stop
// is the documented way to stop it once committed.
func TestPreflight_CommittedIsStoppableViaPlayStop(t *testing.T) {
	g := newGatedTTS(bytes.Repeat([]byte{1}, 32000)) // ~1s of audio
	s, _ := newPreflightServer(t, g)
	close(g.release)
	var sink ttsEventSink
	sink.subscribe(s)

	res, err := s.doPreflightLegTTS("leg-1", preflightReq())
	if err != nil {
		t.Fatalf("doPreflightLegTTS: %v", err)
	}
	if sink.await(events.TTSStaged) == nil {
		t.Fatal("no tts.staged event")
	}
	if _, err := s.doCommitLegTTS("leg-1", res.TTSID); err != nil {
		t.Fatalf("doCommitLegTTS: %v", err)
	}
	if sink.await(events.TTSStarted) == nil {
		t.Fatal("playback never started")
	}
	if _, err := s.doStopLegPlay("leg-1", res.TTSID); err != nil {
		t.Fatalf("doStopLegPlay on a committed tts id: %v", err)
	}
	fin, _ := sink.await(events.TTSFinished).(*events.TTSFinishedData)
	if fin == nil || fin.Reason != "stopped" {
		t.Fatalf("finished = %+v, want reason stopped", fin)
	}
}

func TestPCMDurationMs(t *testing.T) {
	cases := []struct {
		name  string
		audio []byte
		mime  string
		want  int
	}{
		{"16k_pcm", make([]byte, 32000), "audio/pcm;rate=16000", 1000},
		{"8k_pcm", make([]byte, 16000), "audio/l16;rate=8000", 1000},
		{"default_rate", make([]byte, 320), "audio/pcm", 10},
		{"container_format_unknown", make([]byte, 32000), "audio/mpeg", 0},
	}
	for _, tc := range cases {
		if got := pcmDurationMs(tc.audio, tc.mime); got != tc.want {
			t.Errorf("%s: pcmDurationMs = %d, want %d", tc.name, got, tc.want)
		}
	}
}
