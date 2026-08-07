package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/tts"
)

// ttsFake is a scripted tts.Provider. resolveTTSProvider hands it to a
// decorator that calls it from a single goroutine, so a plain counter is
// enough.
type ttsFake struct {
	calls   int
	results []*tts.Result
	errs    []error
}

func (f *ttsFake) Synthesize(ctx context.Context, text string, opts tts.Options) (*tts.Result, error) {
	i := f.calls
	f.calls++
	if i >= len(f.errs) {
		i = len(f.errs) - 1
	}
	var res *tts.Result
	if i < len(f.results) {
		res = f.results[i]
	}
	return res, f.errs[i]
}

// TestResolveTTSProvider_WrapsWithRetry proves the decorator is actually
// installed on the production resolve path — the one thing no test inside
// internal/tts can see. It costs one real backoff (~100ms).
func TestResolveTTSProvider_WrapsWithRetry(t *testing.T) {
	fake := &ttsFake{
		results: []*tts.Result{nil, {Audio: io.NopCloser(strings.NewReader("pcm")), MimeType: "audio/pcm;rate=16000"}},
		errs:    []error{errors.New("elevenlabs: status 503: down"), nil},
	}
	s := &Server{TTS: fake, Config: config.Config{}, Log: slog.Default()}

	p, key := s.resolveTTSProvider(TTSRequest{APIKey: "k"})
	if p == nil {
		t.Fatal("resolveTTSProvider returned a nil provider for a request carrying an api_key")
	}
	if key != "k" {
		t.Fatalf("api key = %q, want %q", key, "k")
	}

	if _, err := p.Synthesize(context.Background(), "hi", tts.Options{}); err != nil {
		t.Fatalf("Synthesize: unexpected error %v", err)
	}
	if fake.calls != 2 {
		t.Fatalf("provider calls = %d, want 2 — the 503 must have been retried", fake.calls)
	}
}

// TestResolveTTSProvider_NilProviderStaysNil guards the "no API key
// configured -> 503" contract: doLegTTS and doRoomTTS decide on a nil
// provider, so the decorator must not manufacture a non-nil wrapper around
// nothing.
func TestResolveTTSProvider_NilProviderStaysNil(t *testing.T) {
	s := &Server{Config: config.Config{}, Log: slog.Default()}

	if p, _ := s.resolveTTSProvider(TTSRequest{APIKey: "k"}); p != nil {
		t.Fatalf("provider = %v, want nil when no TTS provider is configured", p)
	}
}

// ttsAudioLeg is an apiMockLeg with a usable audio writer. apiMockLeg
// hardcodes AudioWriter() to nil, which makes doLegTTS answer 409 before it
// ever reaches synthesis.
type ttsAudioLeg struct {
	*apiMockLeg
}

func (ttsAudioLeg) AudioWriter() io.Writer { return io.Discard }
func (ttsAudioLeg) SampleRate() int        { return 16000 }

// errReader fails its first Read with a non-EOF error, which streamRawPCM
// surfaces as a playback error.
type errReader struct{}

func (errReader) Read(p []byte) (int, error) { return 0, errors.New("stream broke") }
func (errReader) Close() error               { return nil }

const ttsAuthFailure = `azure: status 401: body="invalid key"`

// collectTTSError subscribes to the bus and returns a getter for the first
// tts.error event matching want.
func collectTTSError(t *testing.T, s *Server, match func(*events.TTSErrorData) bool) func() *events.TTSErrorData {
	t.Helper()
	var mu sync.Mutex
	var got *events.TTSErrorData
	s.Bus.Subscribe(func(e events.Event) {
		if e.Type != events.TTSError {
			return
		}
		d, ok := e.Data.(*events.TTSErrorData)
		if !ok || !match(d) {
			return
		}
		mu.Lock()
		if got == nil {
			got = d
		}
		mu.Unlock()
	})
	return func() *events.TTSErrorData {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			d := got
			mu.Unlock()
			if d != nil {
				return d
			}
			time.Sleep(5 * time.Millisecond)
		}
		return nil
	}
}

func TestLegTTSErrorCarriesCategory(t *testing.T) {
	s := newTestServer(t)
	// A 401 categorizes as permanent_auth, so the decorator makes exactly one
	// attempt and the test never sleeps on a backoff.
	s.TTS = &ttsFake{errs: []error{errors.New(ttsAuthFailure)}}
	s.LegMgr.Add(&ttsAudioLeg{&apiMockLeg{id: "tts-leg", createdAt: time.Now()}})

	wait := collectTTSError(t, s, func(d *events.TTSErrorData) bool { return d.LegID == "tts-leg" })

	if _, err := s.doLegTTS("tts-leg", TTSRequest{Text: "hi", Voice: "v", APIKey: "k"}); err != nil {
		t.Fatalf("doLegTTS: %v", err)
	}

	d := wait()
	if d == nil {
		t.Fatal("no tts.error published for the leg")
	}
	if d.Category != string(tts.CategoryPermanentAuth) {
		t.Fatalf("category = %q, want %q", d.Category, tts.CategoryPermanentAuth)
	}
}

// TestRoomTTSErrorCarriesCategory is the neighbour-case guard: the leg test
// above stays green whether or not the structurally identical room publish
// site was updated.
func TestRoomTTSErrorCarriesCategory(t *testing.T) {
	s := newTestServer(t)
	s.TTS = &ttsFake{errs: []error{errors.New(ttsAuthFailure)}}
	s.LegMgr.Add(&apiMockLeg{id: "room-leg", createdAt: time.Now()})
	if _, err := s.RoomMgr.Create("r1", "", 0); err != nil {
		t.Fatalf("Create room: %v", err)
	}
	if err := s.RoomMgr.AddLeg("r1", "room-leg"); err != nil {
		t.Fatalf("AddLeg: %v", err)
	}

	wait := collectTTSError(t, s, func(d *events.TTSErrorData) bool { return d.RoomID == "r1" })

	if _, err := s.doRoomTTS("r1", TTSRequest{Text: "hi", Voice: "v", APIKey: "k"}); err != nil {
		t.Fatalf("doRoomTTS: %v", err)
	}

	d := wait()
	if d == nil {
		t.Fatal("no tts.error published for the room")
	}
	if d.Category != string(tts.CategoryPermanentAuth) {
		t.Fatalf("category = %q, want %q", d.Category, tts.CategoryPermanentAuth)
	}
}

func TestLegTTSPlaybackErrorCategoryIsPlayback(t *testing.T) {
	s := newTestServer(t)
	s.TTS = &ttsFake{
		results: []*tts.Result{{Audio: errReader{}, MimeType: "audio/pcm;rate=16000"}},
		errs:    []error{nil},
	}
	s.LegMgr.Add(&ttsAudioLeg{&apiMockLeg{id: "play-leg", createdAt: time.Now()}})

	wait := collectTTSError(t, s, func(d *events.TTSErrorData) bool { return d.LegID == "play-leg" })

	if _, err := s.doLegTTS("play-leg", TTSRequest{Text: "hi", Voice: "v", APIKey: "k"}); err != nil {
		t.Fatalf("doLegTTS: %v", err)
	}

	d := wait()
	if d == nil {
		t.Fatal("no tts.error published for the leg playback failure")
	}
	if d.Category != string(tts.CategoryPlayback) {
		t.Fatalf("category = %q, want %q", d.Category, tts.CategoryPlayback)
	}
}

// TestRoomTTSPlaybackErrorCategoryIsPlayback is the neighbour case for the
// playback pair, for the same reason the room synth test exists.
func TestRoomTTSPlaybackErrorCategoryIsPlayback(t *testing.T) {
	s := newTestServer(t)
	s.TTS = &ttsFake{
		results: []*tts.Result{{Audio: errReader{}, MimeType: "audio/pcm;rate=16000"}},
		errs:    []error{nil},
	}
	s.LegMgr.Add(&apiMockLeg{id: "play-room-leg", createdAt: time.Now()})
	if _, err := s.RoomMgr.Create("r2", "", 0); err != nil {
		t.Fatalf("Create room: %v", err)
	}
	if err := s.RoomMgr.AddLeg("r2", "play-room-leg"); err != nil {
		t.Fatalf("AddLeg: %v", err)
	}

	wait := collectTTSError(t, s, func(d *events.TTSErrorData) bool { return d.RoomID == "r2" })

	if _, err := s.doRoomTTS("r2", TTSRequest{Text: "hi", Voice: "v", APIKey: "k"}); err != nil {
		t.Fatalf("doRoomTTS: %v", err)
	}

	d := wait()
	if d == nil {
		t.Fatal("no tts.error published for the room playback failure")
	}
	if d.Category != string(tts.CategoryPlayback) {
		t.Fatalf("category = %q, want %q", d.Category, tts.CategoryPlayback)
	}
}
