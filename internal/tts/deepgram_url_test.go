package tts

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDeepgramTTS_PostsToOverriddenBaseURL(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		w.Header().Set("Content-Type", "audio/l16")
		w.Write([]byte{0x01, 0x02, 0x03, 0x04})
	}))
	defer srv.Close()

	d := NewDeepgram("token", srv.URL+"/v1/speak", slog.Default())
	res, err := d.Synthesize(context.Background(), "hello", Options{})
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	defer res.Audio.Close()

	audio, err := io.ReadAll(res.Audio)
	if err != nil {
		t.Fatalf("read audio: %v", err)
	}
	if len(audio) != 4 {
		t.Errorf("audio = %d bytes, want 4", len(audio))
	}
	if gotPath != "/v1/speak" {
		t.Errorf("path = %q, want /v1/speak", gotPath)
	}
	for _, want := range []string{"encoding=linear16", "sample_rate=16000", "container=none"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query %q missing %q", gotQuery, want)
		}
	}
}

func TestDeepgramTTS_EmptyBaseURLKeepsPublicEndpoint(t *testing.T) {
	if got := NewDeepgram("token", "", slog.Default()).baseURL; got != DefaultDeepgramTTSURL {
		t.Errorf("baseURL = %q, want %q", got, DefaultDeepgramTTSURL)
	}
}
