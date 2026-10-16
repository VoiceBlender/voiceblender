package tts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/VoiceBlender/voiceblender/internal/providercfg"
)

// Deepgram implements Provider using the Deepgram TTS API.
type Deepgram struct {
	apiKey string
	cfg    providercfg.Deepgram
	client *http.Client
	log    *slog.Logger
}

// NewDeepgram creates a Deepgram TTS provider.
func NewDeepgram(apiKey string, cfg providercfg.Deepgram, log *slog.Logger) *Deepgram {
	return &Deepgram{
		apiKey: apiKey,
		cfg:    cfg,
		client: &http.Client{},
		log:    log,
	}
}

func (d *Deepgram) Synthesize(ctx context.Context, text string, opts Options) (*Result, error) {
	apiKey := opts.APIKey
	if apiKey == "" {
		apiKey = d.apiKey
	}
	if apiKey == "" {
		return nil, fmt.Errorf("deepgram: no API key provided")
	}

	model := opts.Voice
	if model == "" {
		model = "aura-2-asteria-en"
	}

	body, err := json.Marshal(map[string]string{
		"text": text,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, buildDeepgramSpeakURL(d.cfg, model), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "token "+apiKey)

	d.log.Debug("deepgram synthesize", "model", model, "text_len", len(text))

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("deepgram request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("deepgram: status %d: %s", resp.StatusCode, string(errBody))
	}

	return &Result{
		Audio:    resp.Body,
		MimeType: "audio/pcm;rate=16000",
	}, nil
}

func buildDeepgramSpeakURL(cfg providercfg.Deepgram, model string) string {
	u := cfg.HTTPBase() + "/v1/speak?model=" + url.QueryEscape(model) + "&encoding=linear16&sample_rate=16000&container=none"
	if cfg.MIPOptOut {
		u += "&mip_opt_out=true"
	}
	return u
}
