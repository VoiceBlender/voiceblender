package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/config"
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
