package tts

import (
	"context"
	"io"
)

// Options controls TTS synthesis parameters.
type Options struct {
	Voice    string // provider-specific voice identifier
	ModelID  string // optional, provider-specific model
	Language string // optional, language code (e.g. "en-US", "pl-pl")
	Prompt   string // optional, style/tone instruction (Google Gemini TTS)
	APIKey   string // per-request API key override
}

// Result holds the synthesized audio stream.
type Result struct {
	Audio    io.ReadCloser
	MimeType string // "audio/mpeg", "audio/wav", etc.
}

// Provider synthesizes text into an audio stream.
type Provider interface {
	Synthesize(ctx context.Context, text string, opts Options) (*Result, error)
}
