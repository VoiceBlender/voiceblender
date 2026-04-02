// Package amd implements classic Answering Machine Detection (AMD) by
// analysing the first few seconds of audio on an outbound call. It measures
// initial silence, greeting duration and speech/silence patterns to classify
// the answerer as human, machine, no-speech or not-sure.
package amd

import (
	"context"
	"encoding/binary"
	"io"
	"math"
	"time"
)

// Result is the AMD classification outcome.
type Result string

const (
	ResultHuman    Result = "human"
	ResultMachine  Result = "machine"
	ResultNoSpeech Result = "no_speech"
	ResultNotSure  Result = "not_sure"
)

// Params controls the AMD analysis thresholds.
type Params struct {
	InitialSilenceTimeout time.Duration // max silence before no_speech
	GreetingDuration      time.Duration // speech length threshold for machine
	AfterGreetingSilence  time.Duration // silence after speech to declare human
	TotalAnalysisTime     time.Duration // hard analysis deadline
	MinimumWordLength     time.Duration // min speech burst to count as a word

	// Beep detection — after classifying "machine", continue listening for
	// the voicemail beep tone so the caller knows when to start speaking.
	// Set to 0 to disable beep detection (default).
	BeepTimeout time.Duration // max time to wait for beep after machine detection
}

// Detection holds the AMD result and timing measurements.
type Detection struct {
	Result             Result
	InitialSilenceMs   int
	GreetingDurationMs int
	TotalAnalysisMs    int
}

// BeepResult holds the outcome of beep detection after a machine classification.
type BeepResult struct {
	Detected bool
	BeepMs   int // ms from start of beep waiting to detection (0 if not detected)
}

// Analyzer performs answering machine detection on a 16 kHz PCM audio stream.
type Analyzer struct {
	params Params
}

// New creates an Analyzer with the given parameters.
func New(params Params) *Analyzer {
	return &Analyzer{params: params}
}

// Params returns the analyzer's configuration.
func (a *Analyzer) Params() Params { return a.params }

// analysis state phases
type phase int

const (
	phaseWaitingForSpeech phase = iota
	phaseInGreeting
	phaseAfterGreetingSilence
)

// Audio constants (16 kHz, 16-bit mono, 20 ms frames).
const (
	sampleRate      = 16000
	frameDuration   = 20 * time.Millisecond
	samplesPerFrame = sampleRate * 20 / 1000 // 320
	frameSizeBytes  = samplesPerFrame * 2    // 640

	// Voice activity thresholds — tighter debouncing than the mixer's
	// speaking detection because AMD needs faster reaction times.
	speechThreshold = 300 // RMS level
	speechOnFrames  = 2   // consecutive voiced frames to confirm speech (40 ms)
	speechOffFrames = 4   // consecutive silent frames to confirm silence (80 ms)
)

// Run blocks while reading 16 kHz 16-bit mono PCM from reader, analysing up
// to TotalAnalysisTime of audio. It returns a Detection when a determination
// is made or the context is cancelled.
func (a *Analyzer) Run(ctx context.Context, reader io.Reader) Detection {
	buf := make([]byte, frameSizeBytes)
	samples := make([]int16, samplesPerFrame)

	currentPhase := phaseWaitingForSpeech

	var (
		elapsed        time.Duration // total analysis time
		initialSilence time.Duration // silence before first speech
		greetingDur    time.Duration // cumulative speech duration
		currentSilence time.Duration // current silence streak
		currentSpeech  time.Duration // current speech streak
		activeFrames   int           // consecutive voiced frames
		silentFrames   int           // consecutive silent frames
		speaking       bool          // debounced speech state
	)

	for {
		if ctx.Err() != nil {
			return Detection{
				Result:             ResultNotSure,
				TotalAnalysisMs:    ms(elapsed),
				InitialSilenceMs:   ms(initialSilence),
				GreetingDurationMs: ms(greetingDur),
			}
		}

		_, err := io.ReadFull(reader, buf)
		if err != nil {
			return Detection{
				Result:             ResultNotSure,
				TotalAnalysisMs:    ms(elapsed),
				InitialSilenceMs:   ms(initialSilence),
				GreetingDurationMs: ms(greetingDur),
			}
		}

		// Decode PCM bytes to int16 samples.
		for i := range samples {
			samples[i] = int16(binary.LittleEndian.Uint16(buf[i*2 : i*2+2]))
		}

		rms := computeRMS(samples)
		elapsed += frameDuration

		// Update debounced voice activity state.
		if rms >= speechThreshold {
			activeFrames++
			silentFrames = 0
		} else {
			silentFrames++
			activeFrames = 0
		}

		wasSpeaking := speaking
		if !speaking && activeFrames >= speechOnFrames {
			speaking = true
		} else if speaking && silentFrames >= speechOffFrames {
			speaking = false
		}

		// Hard deadline check.
		if elapsed >= a.params.TotalAnalysisTime {
			return Detection{
				Result:             ResultNotSure,
				InitialSilenceMs:   ms(initialSilence),
				GreetingDurationMs: ms(greetingDur),
				TotalAnalysisMs:    ms(elapsed),
			}
		}

		switch currentPhase {
		case phaseWaitingForSpeech:
			if speaking {
				currentPhase = phaseInGreeting
				currentSpeech = frameDuration
				greetingDur = frameDuration
			} else {
				initialSilence += frameDuration
				if initialSilence >= a.params.InitialSilenceTimeout {
					return Detection{
						Result:           ResultNoSpeech,
						InitialSilenceMs: ms(initialSilence),
						TotalAnalysisMs:  ms(elapsed),
					}
				}
			}

		case phaseInGreeting:
			if speaking {
				currentSpeech += frameDuration
				greetingDur += frameDuration
				currentSilence = 0

				// Long continuous/cumulative speech → machine.
				if greetingDur >= a.params.GreetingDuration {
					return Detection{
						Result:             ResultMachine,
						InitialSilenceMs:   ms(initialSilence),
						GreetingDurationMs: ms(greetingDur),
						TotalAnalysisMs:    ms(elapsed),
					}
				}
			} else {
				// Transition from speaking to silent.
				if wasSpeaking && !speaking {
					// Only count the speech burst if it met minimum word length.
					if currentSpeech < a.params.MinimumWordLength {
						// Too short — treat as noise, don't count towards greeting.
						greetingDur -= currentSpeech
					}
					currentSpeech = 0
				}
				currentSilence += frameDuration

				// Silence after a short greeting → human.
				if currentSilence >= a.params.AfterGreetingSilence {
					if greetingDur > 0 {
						return Detection{
							Result:             ResultHuman,
							InitialSilenceMs:   ms(initialSilence),
							GreetingDurationMs: ms(greetingDur),
							TotalAnalysisMs:    ms(elapsed),
						}
					}
					// No qualifying speech was counted (all bursts too short).
					// Fall back to waiting for speech, carrying forward silence.
					currentPhase = phaseWaitingForSpeech
					initialSilence += currentSilence
					currentSilence = 0
				}
			}

		case phaseAfterGreetingSilence:
			// This phase is handled inline in phaseInGreeting above via
			// currentSilence tracking. Kept as a named constant for clarity.
		}
	}
}

// Beep detector defaults.
const (
	beepMinFreq     = 800.0  // Hz — lower bound of beep frequency range
	beepMaxFreq     = 1200.0 // Hz — upper bound of beep frequency range
	beepEnergyRatio = 0.2    // 20% of frame energy must be in target band
	beepMinFrames   = 4      // 4 × 20ms = 80ms of sustained tone to confirm
)

// WaitForBeep continues reading audio after a "machine" classification,
// looking for the voicemail beep tone (800–1200 Hz). It blocks until the beep
// is found, the timeout expires, or the context is cancelled.
func (a *Analyzer) WaitForBeep(ctx context.Context, reader io.Reader) BeepResult {
	bd := newBeepDetector(beepMinFreq, beepMaxFreq, beepEnergyRatio, beepMinFrames)
	buf := make([]byte, frameSizeBytes)
	samples := make([]int16, samplesPerFrame)

	deadline := a.params.BeepTimeout
	var waited time.Duration

	for waited < deadline {
		if ctx.Err() != nil {
			return BeepResult{}
		}

		_, err := io.ReadFull(reader, buf)
		if err != nil {
			return BeepResult{}
		}

		for i := range samples {
			samples[i] = int16(binary.LittleEndian.Uint16(buf[i*2 : i*2+2]))
		}

		waited += frameDuration

		if bd.feed(samples) {
			return BeepResult{Detected: true, BeepMs: ms(waited)}
		}
	}
	return BeepResult{}
}

// computeRMS returns the root-mean-square of int16 PCM samples.
// Same formula as internal/mixer/speaking.go.
func computeRMS(samples []int16) float64 {
	if len(samples) == 0 {
		return 0
	}
	var sum float64
	for _, s := range samples {
		sum += float64(s) * float64(s)
	}
	return math.Sqrt(sum / float64(len(samples)))
}

func ms(d time.Duration) int {
	return int(d.Milliseconds())
}
