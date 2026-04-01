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

// Validate checks that all parameters are positive and internally consistent.
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
	if p.TotalAnalysisTime < p.InitialSilenceTimeout {
		return errors.New("total_analysis_time must be >= initial_silence_timeout")
	}
	return nil
}

// MergeMillis builds Params by starting from defaults, then overriding with
// any non-zero millisecond values from the per-call API request.
func MergeMillis(defaults Params, initialSilenceMs, greetingMs, afterGreetingMs, totalMs, minWordMs, beepTimeoutMs int) Params {
	p := defaults
	if initialSilenceMs > 0 {
		p.InitialSilenceTimeout = time.Duration(initialSilenceMs) * time.Millisecond
	}
	if greetingMs > 0 {
		p.GreetingDuration = time.Duration(greetingMs) * time.Millisecond
	}
	if afterGreetingMs > 0 {
		p.AfterGreetingSilence = time.Duration(afterGreetingMs) * time.Millisecond
	}
	if totalMs > 0 {
		p.TotalAnalysisTime = time.Duration(totalMs) * time.Millisecond
	}
	if minWordMs > 0 {
		p.MinimumWordLength = time.Duration(minWordMs) * time.Millisecond
	}
	if beepTimeoutMs > 0 {
		p.BeepTimeout = time.Duration(beepTimeoutMs) * time.Millisecond
	}
	return p
}
