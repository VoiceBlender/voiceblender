package tts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
)

const (
	elevenLabsBaseURL    = "https://api.elevenlabs.io/v1/text-to-speech"
	elevenLabsDefaultModel = "eleven_multilingual_v2"
)

// ElevenLabs implements Provider using the ElevenLabs streaming TTS API.
type ElevenLabs struct {
	apiKey string
	client *http.Client
	log    *slog.Logger
}

// NewElevenLabs creates an ElevenLabs TTS provider.
func NewElevenLabs(apiKey string, log *slog.Logger) *ElevenLabs {
	return &ElevenLabs{
		apiKey: apiKey,
		client: &http.Client{},
		log:    log,
	}
}

func (e *ElevenLabs) Synthesize(ctx context.Context, text string, opts Options) (*Result, error) {
	apiKey := opts.APIKey
	if apiKey == "" {
		apiKey = e.apiKey
	}
	if apiKey == "" {
		return nil, fmt.Errorf("elevenlabs: no API key provided")
	}

	model := opts.ModelID
	if model == "" {
		model = elevenLabsDefaultModel
	}

	body, err := json.Marshal(map[string]string{
		"text":     text,
		"model_id": model,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/%s/stream?output_format=pcm_16000", elevenLabsBaseURL, opts.Voice)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("xi-api-key", apiKey)

	e.log.Debug("elevenlabs synthesize", "voice", opts.Voice, "model", model, "text_len", len(text))

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("elevenlabs request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("elevenlabs: status %d: %s", resp.StatusCode, string(errBody))
	}

	return &Result{
		Audio:    resp.Body,
		MimeType: "audio/pcm;rate=16000",
	}, nil
}
