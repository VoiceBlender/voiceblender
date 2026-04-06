//go:build integration

package integration

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/tts"
)

func mustNewCache(t *testing.T) *tts.Cache {
	t.Helper()
	c, err := tts.NewCache(t.TempDir(), false, slog.Default())
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	return c
}

// mockTTSProvider is a minimal tts.Provider that counts Synthesize invocations.
type mockTTSProvider struct {
	calls    atomic.Int64
	mimeType string
	// If errOnCall > 0, the first errOnCall calls return an error.
	errOnCall int64
	audioData string
}

func (m *mockTTSProvider) Synthesize(_ context.Context, _ string, _ tts.Options) (*tts.Result, error) {
	n := m.calls.Add(1)
	if m.errOnCall > 0 && n <= m.errOnCall {
		return nil, errors.New("mock synthesis error")
	}
	mime := m.mimeType
	if mime == "" {
		mime = "audio/mpeg"
	}
	data := m.audioData
	if data == "" {
		data = "audio data"
	}
	return &tts.Result{
		Audio:    io.NopCloser(strings.NewReader(data)),
		MimeType: mime,
	}, nil
}

func TestTTSCache_HitAndMiss(t *testing.T) {
	cache := mustNewCache(t)
	mock := &mockTTSProvider{}
	provider := cache.WrapProvider(mock, "testprovider")

	ctx := context.Background()
	opts := tts.Options{Voice: "voice1", ModelID: "model1"}

	// First call — should hit the underlying provider.
	res1, err := provider.Synthesize(ctx, "hello world", opts)
	if err != nil {
		t.Fatalf("first Synthesize: %v", err)
	}
	data1, _ := io.ReadAll(res1.Audio)
	res1.Audio.Close()

	if mock.calls.Load() != 1 {
		t.Fatalf("expected 1 provider call after first Synthesize, got %d", mock.calls.Load())
	}

	// Second call with same parameters — should be served from cache.
	res2, err := provider.Synthesize(ctx, "hello world", opts)
	if err != nil {
		t.Fatalf("second Synthesize: %v", err)
	}
	data2, _ := io.ReadAll(res2.Audio)
	res2.Audio.Close()

	if mock.calls.Load() != 1 {
		t.Fatalf("expected still 1 provider call after cache hit, got %d", mock.calls.Load())
	}
	if string(data1) != string(data2) {
		t.Fatalf("cache hit returned different audio: got %q want %q", string(data2), string(data1))
	}

	// Third call with different text — cache miss, provider must be called again.
	res3, err := provider.Synthesize(ctx, "different text", opts)
	if err != nil {
		t.Fatalf("third Synthesize: %v", err)
	}
	io.ReadAll(res3.Audio)
	res3.Audio.Close()

	if mock.calls.Load() != 2 {
		t.Fatalf("expected 2 provider calls after cache miss, got %d", mock.calls.Load())
	}
	if cache.Len() != 2 {
		t.Fatalf("expected cache.Len() == 2, got %d", cache.Len())
	}
}

func TestTTSCache_MimeTypePreserved(t *testing.T) {
	cache := mustNewCache(t)
	mock := &mockTTSProvider{mimeType: "audio/mpeg"}
	provider := cache.WrapProvider(mock, "testprovider")

	ctx := context.Background()
	opts := tts.Options{Voice: "voice1"}

	res1, err := provider.Synthesize(ctx, "hello", opts)
	if err != nil {
		t.Fatalf("first Synthesize: %v", err)
	}
	io.ReadAll(res1.Audio)
	res1.Audio.Close()
	if res1.MimeType != "audio/mpeg" {
		t.Fatalf("first call MimeType: got %q want %q", res1.MimeType, "audio/mpeg")
	}

	res2, err := provider.Synthesize(ctx, "hello", opts)
	if err != nil {
		t.Fatalf("second Synthesize: %v", err)
	}
	io.ReadAll(res2.Audio)
	res2.Audio.Close()
	if res2.MimeType != "audio/mpeg" {
		t.Fatalf("cache hit MimeType: got %q want %q", res2.MimeType, "audio/mpeg")
	}
}

func TestTTSCache_ErrorNotCached(t *testing.T) {
	cache := mustNewCache(t)
	// First call returns an error; subsequent calls succeed.
	mock := &mockTTSProvider{errOnCall: 1}
	provider := cache.WrapProvider(mock, "testprovider")

	ctx := context.Background()
	opts := tts.Options{Voice: "voice1"}

	// First call — provider returns error, must not be cached.
	_, err := provider.Synthesize(ctx, "hello", opts)
	if err == nil {
		t.Fatal("expected error on first call, got nil")
	}
	if cache.Len() != 0 {
		t.Fatalf("expected cache.Len() == 0 after error, got %d", cache.Len())
	}

	// Second call — provider now succeeds; must reach provider again (error was not cached).
	res, err := provider.Synthesize(ctx, "hello", opts)
	if err != nil {
		t.Fatalf("second Synthesize: %v", err)
	}
	io.ReadAll(res.Audio)
	res.Audio.Close()

	if mock.calls.Load() != 2 {
		t.Fatalf("expected 2 provider calls (error not cached), got %d", mock.calls.Load())
	}
	if cache.Len() != 1 {
		t.Fatalf("expected cache.Len() == 1 after success, got %d", cache.Len())
	}
}

func TestTTSCache_APIKeyIsolation(t *testing.T) {
	// When includeAPIKey=true, different API keys must produce separate entries.
	c, err := tts.NewCache(t.TempDir(), true, slog.Default())
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	mock := &mockTTSProvider{}
	provider := c.WrapProvider(mock, "testprovider")

	ctx := context.Background()

	res1, err := provider.Synthesize(ctx, "hello", tts.Options{Voice: "v1", APIKey: "key-a"})
	if err != nil {
		t.Fatalf("first Synthesize: %v", err)
	}
	io.ReadAll(res1.Audio)
	res1.Audio.Close()

	// Same text/voice, different key — must be a cache miss.
	res2, err := provider.Synthesize(ctx, "hello", tts.Options{Voice: "v1", APIKey: "key-b"})
	if err != nil {
		t.Fatalf("second Synthesize: %v", err)
	}
	io.ReadAll(res2.Audio)
	res2.Audio.Close()

	if mock.calls.Load() != 2 {
		t.Fatalf("expected 2 provider calls (different API keys), got %d", mock.calls.Load())
	}
	if c.Len() != 2 {
		t.Fatalf("expected cache.Len() == 2, got %d", c.Len())
	}

	// Same key again — must be a cache hit.
	res3, err := provider.Synthesize(ctx, "hello", tts.Options{Voice: "v1", APIKey: "key-a"})
	if err != nil {
		t.Fatalf("third Synthesize: %v", err)
	}
	io.ReadAll(res3.Audio)
	res3.Audio.Close()

	if mock.calls.Load() != 2 {
		t.Fatalf("expected still 2 provider calls after cache hit, got %d", mock.calls.Load())
	}
}

func TestTTSCache_APIKeyIgnoredByDefault(t *testing.T) {
	// When includeAPIKey=false (default), different API keys share the same entry.
	cache := mustNewCache(t) // includeAPIKey=false
	mock := &mockTTSProvider{}
	provider := cache.WrapProvider(mock, "testprovider")

	ctx := context.Background()

	res1, err := provider.Synthesize(ctx, "hello", tts.Options{Voice: "v1", APIKey: "key-a"})
	if err != nil {
		t.Fatalf("first Synthesize: %v", err)
	}
	io.ReadAll(res1.Audio)
	res1.Audio.Close()

	// Different key — should still be a cache hit because key is excluded.
	res2, err := provider.Synthesize(ctx, "hello", tts.Options{Voice: "v1", APIKey: "key-b"})
	if err != nil {
		t.Fatalf("second Synthesize: %v", err)
	}
	io.ReadAll(res2.Audio)
	res2.Audio.Close()

	if mock.calls.Load() != 1 {
		t.Fatalf("expected 1 provider call (API key excluded from key), got %d", mock.calls.Load())
	}
	if cache.Len() != 1 {
		t.Fatalf("expected cache.Len() == 1, got %d", cache.Len())
	}
}

func TestTTSCache_ProviderNameIsolation(t *testing.T) {
	cache := mustNewCache(t)
	mock := &mockTTSProvider{}

	providerA := cache.WrapProvider(mock, "a")
	providerB := cache.WrapProvider(mock, "b")

	ctx := context.Background()
	opts := tts.Options{Voice: "voice1"}

	resA, err := providerA.Synthesize(ctx, "hello", opts)
	if err != nil {
		t.Fatalf("providerA Synthesize: %v", err)
	}
	io.ReadAll(resA.Audio)
	resA.Audio.Close()

	resB, err := providerB.Synthesize(ctx, "hello", opts)
	if err != nil {
		t.Fatalf("providerB Synthesize: %v", err)
	}
	io.ReadAll(resB.Audio)
	resB.Audio.Close()

	if mock.calls.Load() != 2 {
		t.Fatalf("expected 2 provider calls (different provider names = different keys), got %d", mock.calls.Load())
	}
	if cache.Len() != 2 {
		t.Fatalf("expected cache.Len() == 2, got %d", cache.Len())
	}
}
