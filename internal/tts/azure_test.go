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

func newAzureWithServer(t *testing.T, handler http.HandlerFunc) (*Azure, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	// Extract host:port from the test server URL to use as the "region".
	// We override the URL construction by using a custom region that resolves to the test server.
	// Instead, we inject a custom http.Client with a transport that rewrites the URL.
	a := &Azure{
		apiKey: "test-key",
		region: "eastus",
		client: &http.Client{
			Transport: redirectTransport{target: srv.URL},
		},
		log: slog.Default(),
	}
	return a, srv
}

// redirectTransport rewrites all requests to a target URL.
type redirectTransport struct {
	target string
}

func (rt redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	newReq := req.Clone(req.Context())
	u, _ := http.NewRequest("GET", rt.target+req.URL.Path, nil)
	newReq.URL = u.URL
	return http.DefaultTransport.RoundTrip(newReq)
}

func TestAzure_Synthesize(t *testing.T) {
	var gotHeaders http.Header
	var gotBody string
	a, _ := newAzureWithServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Write([]byte("fake-pcm-audio"))
	})

	result, err := a.Synthesize(context.Background(), "Hello world", Options{
		Voice: "en-US-GuyNeural",
	})
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	defer result.Audio.Close()

	audio, _ := io.ReadAll(result.Audio)
	if string(audio) != "fake-pcm-audio" {
		t.Errorf("audio = %q, want fake-pcm-audio", string(audio))
	}
	if result.MimeType != "audio/pcm;rate=16000" {
		t.Errorf("MimeType = %q, want audio/pcm;rate=16000", result.MimeType)
	}
	if gotHeaders.Get("Content-Type") != "application/ssml+xml" {
		t.Errorf("Content-Type = %q, want application/ssml+xml", gotHeaders.Get("Content-Type"))
	}
	if gotHeaders.Get("X-Microsoft-OutputFormat") != "raw-16khz-16bit-mono-pcm" {
		t.Errorf("X-Microsoft-OutputFormat = %q, want raw-16khz-16bit-mono-pcm", gotHeaders.Get("X-Microsoft-OutputFormat"))
	}
	if gotHeaders.Get("Ocp-Apim-Subscription-Key") != "test-key" {
		t.Errorf("Ocp-Apim-Subscription-Key = %q, want test-key", gotHeaders.Get("Ocp-Apim-Subscription-Key"))
	}
	if !strings.Contains(gotBody, "en-US-GuyNeural") {
		t.Errorf("SSML body missing voice name: %s", gotBody)
	}
	if !strings.Contains(gotBody, "Hello world") {
		t.Errorf("SSML body missing text: %s", gotBody)
	}
	if !strings.Contains(gotBody, "xml:lang='en-US'") {
		t.Errorf("SSML body missing lang: %s", gotBody)
	}
}

func TestAzure_DefaultVoice(t *testing.T) {
	var gotBody string
	a, _ := newAzureWithServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Write([]byte("audio"))
	})

	result, err := a.Synthesize(context.Background(), "test", Options{})
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	result.Audio.Close()

	if !strings.Contains(gotBody, "en-US-JennyNeural") {
		t.Errorf("expected default voice en-US-JennyNeural in SSML: %s", gotBody)
	}
}

func TestAzure_PerRequestAPIKey(t *testing.T) {
	var gotKey string
	a, _ := newAzureWithServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("Ocp-Apim-Subscription-Key")
		w.Write([]byte("audio"))
	})

	result, err := a.Synthesize(context.Background(), "test", Options{
		APIKey: "override-key",
	})
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	result.Audio.Close()

	if gotKey != "override-key" {
		t.Errorf("API key = %q, want override-key", gotKey)
	}
}

func TestAzure_ExplicitLanguage(t *testing.T) {
	var gotBody string
	a, _ := newAzureWithServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Write([]byte("audio"))
	})

	result, err := a.Synthesize(context.Background(), "test", Options{
		Voice:    "en-US-JennyNeural",
		Language: "pl-PL",
	})
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	result.Audio.Close()

	if !strings.Contains(gotBody, "xml:lang='pl-PL'") {
		t.Errorf("expected explicit language pl-PL in SSML: %s", gotBody)
	}
}

func TestAzure_ErrorResponse(t *testing.T) {
	a, _ := newAzureWithServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte("invalid key"))
	})

	_, err := a.Synthesize(context.Background(), "test", Options{})
	if err == nil {
		t.Fatal("expected error for 401 response")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error = %q, want to contain '401'", err.Error())
	}
}

func TestAzure_NoAPIKey(t *testing.T) {
	a := NewAzure("", "eastus", slog.Default())
	_, err := a.Synthesize(context.Background(), "hello", Options{})
	if err == nil {
		t.Fatal("expected error for missing API key")
	}
	if !strings.Contains(err.Error(), "no API key") {
		t.Errorf("error = %q, want 'no API key'", err.Error())
	}
}

func TestExtractAzureLang(t *testing.T) {
	tests := []struct {
		voice string
		want  string
	}{
		{"en-US-JennyNeural", "en-US"},
		{"pl-PL-MarekNeural", "pl-PL"},
		{"de-DE-KatjaNeural", "de-DE"},
		{"unknown", "en-US"},
	}
	for _, tt := range tests {
		got := extractAzureLang(tt.voice)
		if got != tt.want {
			t.Errorf("extractAzureLang(%q) = %q, want %q", tt.voice, got, tt.want)
		}
	}
}

func TestBuildSSML(t *testing.T) {
	ssml := buildSSML("en-US", "en-US-JennyNeural", "Hello & <world>")
	if !strings.Contains(ssml, "&amp;") {
		t.Error("expected & to be escaped")
	}
	if !strings.Contains(ssml, "&lt;world&gt;") {
		t.Error("expected < and > to be escaped")
	}
	if !strings.Contains(ssml, "xml:lang='en-US'") {
		t.Error("expected lang attribute")
	}
	if !strings.Contains(ssml, "name='en-US-JennyNeural'") {
		t.Error("expected voice name")
	}
}
