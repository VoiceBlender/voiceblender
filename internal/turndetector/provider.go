package turndetector

import (
	"context"
	"io"
)

// Provider is the common interface for SmartTurn transport backends.
// Write receives raw PCM bytes (20ms frames, int16 LE, 16kHz) from the leg's
// speaking tap. The provider handles its own buffering (HTTP mode) or frame
// forwarding (WS mode). Start must be called before any Write calls.
type Provider interface {
	io.Writer
	Start(ctx context.Context, onEvent func(Event)) error
	Stop()
	Running() bool
}

// Event is delivered to the caller on each turn decision from SmartTurn.
type Event struct {
	Complete      bool // true = TURN_COMPLETE, false = TURN_INCOMPLETE
	Probability   float64
	ThresholdUsed float64
	// ProcessingMs is SmartTurn's internal evaluation time.
	ProcessingMs int64
	// RoundtripMs is VoiceBlender's end-to-end latency (network + service).
	// Set only by HTTPProvider; WSProvider cannot measure per-event round-trip.
	RoundtripMs int64
}

// Options configures a Provider session.
type Options struct {
	// ServiceURL is the base URL of the SmartTurn microservice (scheme + host + optional port, no path).
	ServiceURL string
	// VAD specifies which VAD engine SmartTurn should use in WS mode: "rms" or "silero" (default "silero").
	VAD string
	// Threshold is the probability cutoff for TURN_COMPLETE decisions (default 0.55).
	Threshold float64
	// PauseDurationMs is how many milliseconds of silence trigger evaluation (default 400, WS mode only).
	PauseDurationMs int
	// Adaptive enables dynamic duration-based adaptive thresholding on the SmartTurn WS endpoint.
	Adaptive bool
	// OnError is an optional callback invoked when an evaluation fails or the session drops.
	OnError func(reason, msg string, fatal bool)
}
