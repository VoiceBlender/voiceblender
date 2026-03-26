package stt

import (
	"context"
	"io"
)

// Provider is the common interface for speech-to-text backends.
type Provider interface {
	Start(ctx context.Context, reader io.Reader, apiKey string,
		opts Options, cb TranscriptCallback) error
	Stop()
	Running() bool
}

// Options configures the transcription session.
type Options struct {
	Language string // ISO-639-1 language code (default "en")
	Partial  bool   // emit partial transcripts
}

// TranscriptCallback is called for each transcript result.
type TranscriptCallback func(text string, isFinal bool)
