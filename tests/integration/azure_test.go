//go:build integration

package integration

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/stt"
	"github.com/VoiceBlender/voiceblender/internal/tts"
)

func TestAzureTTS_Integration(t *testing.T) {
	key := os.Getenv("AZURE_SPEECH_KEY")
	region := os.Getenv("AZURE_SPEECH_REGION")
	if key == "" {
		t.Skip("AZURE_SPEECH_KEY not set, skipping Azure TTS integration test")
	}
	if region == "" {
		region = "eastus"
	}

	provider := tts.NewAzure(key, region, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	result, err := provider.Synthesize(ctx, "Hello from Azure.", tts.Options{
		Voice: "en-US-JennyNeural",
	})
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	defer result.Audio.Close()

	audio, err := io.ReadAll(result.Audio)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	if len(audio) == 0 {
		t.Fatal("expected non-empty audio data")
	}
	if result.MimeType != "audio/pcm;rate=16000" {
		t.Errorf("MimeType = %q, want audio/pcm;rate=16000", result.MimeType)
	}
	t.Logf("Azure TTS returned %d bytes of audio", len(audio))
}

func TestAzureSTT_Integration(t *testing.T) {
	key := os.Getenv("AZURE_SPEECH_KEY")
	region := os.Getenv("AZURE_SPEECH_REGION")
	if key == "" {
		t.Skip("AZURE_SPEECH_KEY not set, skipping Azure STT integration test")
	}
	if region == "" {
		region = "eastus"
	}

	// Generate silence (640 bytes = 20ms at 16kHz) repeated for ~2 seconds.
	frame := make([]byte, 640)
	var audioData []byte
	for i := 0; i < 100; i++ {
		audioData = append(audioData, frame...)
	}

	transcriber := stt.NewAzure(region, slog.Default())

	var transcripts []string
	var mu sync.Mutex
	cb := func(text string, isFinal bool) {
		mu.Lock()
		transcripts = append(transcripts, text)
		mu.Unlock()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	reader := strings.NewReader(string(audioData))
	err := transcriber.Start(ctx, reader, key, stt.Options{Language: "en-US"}, cb)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	t.Logf("Azure STT completed, got %d transcripts", len(transcripts))
}
