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

// Finalizer is implemented by providers that can flush the server-side audio
// buffer mid-stream, forcing a final transcript for what has been spoken so
// far WITHOUT closing the session. Optional: only Deepgram supports it today,
// so callers must type-assert rather than assume every Provider has it.
type Finalizer interface {
	Finalize(ctx context.Context) error
}

// Options configures the transcription session.
type Options struct {
	Language string // ISO-639-1 language code (default "en")
	Partial  bool   // emit partial transcripts
}

// TranscriptCallback is called for each transcript result.
type TranscriptCallback func(text string, isFinal bool)
