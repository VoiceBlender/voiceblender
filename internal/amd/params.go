package amd

import (
	"errors"
	"time"
)

// DefaultParams returns AMD parameters with sensible defaults matching
// industry-standard answering machine detection (Asterisk, FreeSWITCH).
func DefaultParams() Params {
	return Params{
		InitialSilenceTimeout: 2500 * time.Millisecond,
		GreetingDuration:      1500 * time.Millisecond,
		AfterGreetingSilence:  800 * time.Millisecond,
		TotalAnalysisTime:     5000 * time.Millisecond,
		MinimumWordLength:     100 * time.Millisecond,
		BeepTimeout:           0, // disabled by default
	}
}

// Validate checks that the threshold parameters are positive, that BeepTimeout
// is non-negative (0 disables beep detection), and that the windows are
// internally consistent.
func (p Params) Validate() error {
	if p.InitialSilenceTimeout <= 0 {
		return errors.New("initial_silence_timeout must be positive")
	}
	if p.GreetingDuration <= 0 {
		return errors.New("greeting_duration must be positive")
	}
	if p.AfterGreetingSilence <= 0 {
		return errors.New("after_greeting_silence must be positive")
	}
	if p.TotalAnalysisTime <= 0 {
		return errors.New("total_analysis_time must be positive")
	}
	if p.MinimumWordLength <= 0 {
		return errors.New("minimum_word_length must be positive")
	}
	// BeepTimeout is the one field where 0 is legal: it means beep detection
	// is disabled (see DefaultParams). So this guard is `< 0`, not `<= 0`.
	if p.BeepTimeout < 0 {
		return errors.New("beep_timeout must not be negative")
	}
	// A window that a verdict cannot be reached within silently defeats that
	// verdict: every call on the leg falls out as not_sure. Comparing against
	// the raw threshold is not enough. fsmState.step advances one frame at a
	// time, so a verdict fires at the first frame boundary at or past its
	// threshold — a fixed elapsed we call the verdict frame. step runs its
	// hard-deadline check *before* the phase switch that emits the verdict, so
	// the verdict only survives when the deadline falls strictly after that
	// frame. Hence each verdict is reachable exactly when TotalAnalysisTime is
	// strictly greater than its verdict frame, and we reject at or below it.
	if p.TotalAnalysisTime <= analysisFrames(p.InitialSilenceTimeout) {
		return errors.New("total_analysis_time is too short to ever reach no_speech (initial_silence_timeout)")
	}
	// Speech latches only after speechOnFrames voiced frames, and greetingDur
	// starts counting at that frame, so the greeting lags the stream by
	// speechOnFrames-1 frames.
	if p.TotalAnalysisTime <= analysisFrames(p.GreetingDuration)+(speechOnFrames-1)*frameDuration {
		return errors.New("total_analysis_time is too short to ever reach machine (greeting_duration)")
	}
	// A burst must survive the off-debounce to end, and those trailing silent
	// frames still accrue to currentSpeech, so no burst is shorter than
	// speechOffFrames — even when MinimumWordLength is.
	burst := analysisFrames(p.MinimumWordLength)
	if burst < speechOffFrames*frameDuration {
		burst = speechOffFrames * frameDuration
	}
	if p.TotalAnalysisTime <= burst+(speechOnFrames-1)*frameDuration+analysisFrames(p.AfterGreetingSilence) {
		return errors.New("total_analysis_time is too short to ever reach human (after_greeting_silence)")
	}
	return nil
}

// analysisFrames rounds d up to a whole number of analysis frames.
func analysisFrames(d time.Duration) time.Duration {
	return (d + frameDuration - 1) / frameDuration * frameDuration
}

// MergeMillis builds Params by starting from defaults, then overriding with
// any non-zero millisecond values from the per-call API request.
//
// Zero means "not supplied", so it keeps the default. Negative values are
// deliberately carried through rather than ignored: Validate then rejects
// them, so a caller that sends garbage gets an error instead of a silent
// default. Do not narrow these gates back to `> 0` — that hides a negative
// behind the default and makes it indistinguishable from an omitted field.
func MergeMillis(defaults Params, initialSilenceMs, greetingMs, afterGreetingMs, totalMs, minWordMs, beepTimeoutMs int) Params {
	p := defaults
	if initialSilenceMs != 0 {
		p.InitialSilenceTimeout = time.Duration(initialSilenceMs) * time.Millisecond
	}
	if greetingMs != 0 {
		p.GreetingDuration = time.Duration(greetingMs) * time.Millisecond
	}
	if afterGreetingMs != 0 {
		p.AfterGreetingSilence = time.Duration(afterGreetingMs) * time.Millisecond
	}
	if totalMs != 0 {
		p.TotalAnalysisTime = time.Duration(totalMs) * time.Millisecond
	}
	if minWordMs != 0 {
		p.MinimumWordLength = time.Duration(minWordMs) * time.Millisecond
	}
	if beepTimeoutMs != 0 {
		p.BeepTimeout = time.Duration(beepTimeoutMs) * time.Millisecond
	}
	return p
}
