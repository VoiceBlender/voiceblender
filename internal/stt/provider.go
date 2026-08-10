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
// Deepgram Flux does NOT — /v2/listen has no Finalize message.
type Finalizer interface {
	Finalize(ctx context.Context) error
}

// Turn event kinds. The Deepgram Flux state machine emits the first five;
// Deepgram v1 emits only TurnUtteranceEnd.
const (
	TurnStartOfTurn    = "start_of_turn"
	TurnUpdate         = "update"
	TurnEagerEndOfTurn = "eager_end_of_turn"
	TurnResumed        = "turn_resumed"
	TurnEndOfTurn      = "end_of_turn"
	TurnUtteranceEnd   = "utterance_end"
)

// Options configures the transcription session.
type Options struct {
	Language string // ISO-639-1 language code (default "en")
	Partial  bool   // emit partial transcripts

	Model string // provider model override (v1 "nova-3", flux "flux-general-en")

	// Deepgram v1. Pointers because 0 is meaningful (endpointing=0 disables
	// endpointing) and must be distinguishable from "not set".
	Endpointing    *int
	UtteranceEndMs *int

	// Deepgram Flux.
	EagerEOTThreshold *float64
	EOTThreshold      *float64
	EOTTimeoutMs      *int
	LanguageHints     []string

	Keyterms []string

	// Optional sinks. When OnTranscript is non-nil it supersedes the
	// TranscriptCallback passed to Start.
	OnTranscript TranscriptDetailCallback
	OnTurn       TurnCallback
}

// TranscriptCallback is called for each transcript result.
type TranscriptCallback func(text string, isFinal bool)

// TranscriptEvent is the richer transcript payload delivered through
// Options.OnTranscript.
type TranscriptEvent struct {
	Text        string
	IsFinal     bool
	SpeechFinal bool // the speaker stopped, not just a finalized segment
}

// TranscriptDetailCallback receives TranscriptEvent values.
type TranscriptDetailCallback func(TranscriptEvent)

// TurnWord is a single word within a turn, with its timing and confidence.
type TurnWord struct {
	Word       string
	Confidence float64
	Start      float64 // seconds
	End        float64 // seconds
}

// TurnEvent carries a turn-boundary signal. Times are seconds, as the
// provider sends them; the API layer converts to milliseconds.
type TurnEvent struct {
	Event               string // one of the Turn* constants
	TurnIndex           int
	Transcript          string
	EndOfTurnConfidence float64
	AudioWindowStart    float64
	AudioWindowEnd      float64
	LastWordEnd         float64 // Deepgram v1 UtteranceEnd only
	Words               []TurnWord
	Languages           []string
}

// TurnCallback receives TurnEvent values.
type TurnCallback func(TurnEvent)

// emitTranscript delivers a transcript to whichever sink the caller supplied.
// OnTranscript wins so a caller that wants speech_final never also gets the
// lossy two-argument form.
func emitTranscript(opts Options, cb TranscriptCallback, ev TranscriptEvent) {
	if opts.OnTranscript != nil {
		opts.OnTranscript(ev)
		return
	}
	if cb != nil {
		cb(ev.Text, ev.IsFinal)
	}
}

// emitTurn delivers a turn event, if the caller asked for them.
func emitTurn(opts Options, ev TurnEvent) {
	if opts.OnTurn != nil {
		opts.OnTurn(ev)
	}
}
