package tts

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"time"
)

// Category classifies a synthesis failure so callers can tell a permanent
// misconfiguration apart from a transient upstream blip. It is published on
// the tts.error event, and the value set is open: consumers must treat an
// unrecognised value as "unknown" rather than rejecting the event.
type Category string

const (
	// CategoryPermanentAuth is a rejected or missing credential (401, 403).
	CategoryPermanentAuth Category = "permanent_auth"
	// CategoryPermanentInput is a request the upstream will never accept
	// (400, 404, 422) — a bad voice name, malformed text, wrong model.
	CategoryPermanentInput Category = "permanent_input"
	// CategoryRateLimited is a 429.
	CategoryRateLimited Category = "rate_limited"
	// CategoryServiceUnavailable is an upstream 5xx.
	CategoryServiceUnavailable Category = "service_unavailable"
	// CategoryRetryable is a transient failure that is neither a 429 nor a
	// 5xx: a request timeout (408), a deadline, a transport error.
	CategoryRetryable Category = "retryable"
	// CategoryCanceled means the caller went away — the leg or room context
	// was cancelled. Terminal: there is nobody left to hear the audio.
	CategoryCanceled Category = "canceled"
	// CategoryUnknown is a failure with no status and no recognised transport
	// shape: SDK errors from AWS Polly and Google, and the providers' own
	// "no API key provided" guards.
	CategoryUnknown Category = "unknown"
	// CategoryPlayback is set by the API layer for failures raised after
	// synthesis succeeded, while streaming the audio to the leg or room
	// (internal/api/tts.go, the two playback-error publish sites). Categorize
	// never returns it.
	CategoryPlayback Category = "playback"
)

// retryable reports whether re-issuing the identical request could plausibly
// succeed.
//
// CategoryUnknown is deliberately NOT retryable, diverging from the
// ai-runtime categorizer this was ported from. Two reasons: aws-sdk-go-v2
// already retries 3 times internally (config.LoadDefaultConfig installs the
// standard retryer used by internal/tts/aws.go), so a second loop around it
// would mean up to 9 upstream calls; and the unknown set is dominated by
// misconfiguration — a missing API key, an unparseable credential, a failed
// client construction — which no number of retries can fix.
func (c Category) retryable() bool {
	switch c {
	case CategoryRetryable, CategoryRateLimited, CategoryServiceUnavailable:
		return true
	default:
		return false
	}
}

// ttsStatusRe extracts the HTTP status from the error strings the three HTTP
// providers build: "elevenlabs: status %d: %s", "deepgram: status %d: %s" and
// "azure: status %d: body=%q ...". Only the FIRST match is used, because the
// azure shape embeds up to 2048 bytes of the response body after the real
// status and that body can itself contain a "status NNN" token.
var ttsStatusRe = regexp.MustCompile(`status (\d{3})`)

// Categorize classifies a Synthesize error. It returns "" for a nil error.
//
// The order is load-bearing. context.Canceled and context.DeadlineExceeded are
// checked before the transport arms because http.Client.Do returns a
// *url.Error wrapping the context error on cancellation, and every provider
// wraps that with %w — so checking *url.Error first would classify a hung-up
// caller as a retryable transport blip and keep calling the upstream.
func Categorize(err error) Category {
	if err == nil {
		return ""
	}

	if errors.Is(err, context.Canceled) {
		return CategoryCanceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return CategoryRetryable
	}

	if m := ttsStatusRe.FindStringSubmatch(err.Error()); m != nil {
		if code, convErr := strconv.Atoi(m[1]); convErr == nil {
			switch code {
			case 401, 403:
				return CategoryPermanentAuth
			case 400, 404, 422:
				return CategoryPermanentInput
			case 408:
				return CategoryRetryable
			case 429:
				return CategoryRateLimited
			case 500, 502, 503, 504:
				return CategoryServiceUnavailable
			default:
				return CategoryUnknown
			}
		}
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return CategoryRetryable
	}

	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return CategoryRetryable
	}

	return CategoryUnknown
}

// Retry policy. Deliberately constants rather than configuration: the numbers
// are bounded by ttsRetryMaxElapsed, so there is nothing an operator would
// need to tune without also changing the code that consumes them.
const (
	ttsRetryMaxAttempts = 3
	// ttsRetryBaseInterval is the pre-jitter delay before the second attempt.
	ttsRetryBaseInterval = 100 * time.Millisecond
	ttsRetryMultiplier   = 4
	// ttsRetryMaxInterval is the PRE-JITTER ceiling on a single delay; jitter
	// is applied afterwards, so the effective maximum is 25% higher.
	ttsRetryMaxInterval = 1600 * time.Millisecond
	// ttsRetryJitterFraction spreads each delay over ±25% so concurrent legs
	// failing on the same upstream blip do not retry in lockstep.
	ttsRetryJitterFraction = 0.25
	// ttsRetryMaxElapsed bounds the whole loop, not one delay: once the next
	// delay would push past it, the last error is returned instead.
	ttsRetryMaxElapsed = 5 * time.Second
)

// retryDelay returns the jittered backoff before the attempt following the
// given zero-based attempt index.
//
// The exponent is applied by repeated multiplication with an early clamp
// rather than math.Pow, so a large attempt index can neither overflow int64
// nor produce an absurd duration.
func retryDelay(attempt int, base, maxInterval time.Duration) time.Duration {
	d := base
	for i := 0; i < attempt; i++ {
		if d > maxInterval/ttsRetryMultiplier {
			d = maxInterval
			break
		}
		d *= ttsRetryMultiplier
	}
	jitter := (rand.Float64()*2 - 1) * ttsRetryJitterFraction
	return time.Duration(float64(d) * (1 + jitter))
}

// retryingProvider re-issues a failed Synthesize when the failure looks
// transient. It is immutable after construction and spawns no goroutines, so
// it adds no synchronization and nothing to leak.
//
// Retrying a whole Synthesize is safe for every provider in this package:
//
//   - All three HTTP providers check resp.StatusCode BEFORE handing resp.Body
//     back as Result.Audio (elevenlabs.go:71 vs :77-80, azure.go:77 vs
//     :101-104, deepgram.go:67 vs :73-76), so a non-nil error guarantees that
//     zero audio bytes ever reached the caller.
//   - Each provider builds a fresh request from an in-memory body on every
//     call (elevenlabs.go:57 bytes.NewReader, azure.go:53 strings.NewReader,
//     deepgram.go:53 bytes.NewReader), so there is no consumed reader to
//     rewind between attempts.
type retryingProvider struct {
	inner       Provider
	name        string
	log         *slog.Logger
	maxAttempts int
	base        time.Duration
	maxInterval time.Duration
	maxElapsed  time.Duration
}

// NewRetrying wraps inner so transient synthesis failures are retried under
// the package retry policy. name is the provider name, used only for logging.
func NewRetrying(inner Provider, name string, log *slog.Logger) Provider {
	return &retryingProvider{
		inner:       inner,
		name:        name,
		log:         log,
		maxAttempts: ttsRetryMaxAttempts,
		base:        ttsRetryBaseInterval,
		maxInterval: ttsRetryMaxInterval,
		maxElapsed:  ttsRetryMaxElapsed,
	}
}

// Synthesize calls the wrapped provider, retrying transient failures.
//
// On success the inner *Result is returned unchanged, so the audio stream is
// never copied or re-wrapped. On failure the last attempt's error is returned
// by identity — not reformatted — so errors.Is, errors.As and any status
// substring a caller matches on all survive the decorator.
func (r *retryingProvider) Synthesize(ctx context.Context, text string, opts Options) (*Result, error) {
	deadline := time.Now().Add(r.maxElapsed)
	var lastErr error

	for attempt := 0; ; attempt++ {
		res, err := r.inner.Synthesize(ctx, text, opts)
		if err == nil {
			return res, nil
		}
		lastErr = err

		cat := Categorize(err)
		if !cat.retryable() || attempt >= r.maxAttempts-1 {
			break
		}

		d := retryDelay(attempt, r.base, r.maxInterval)
		if time.Now().Add(d).After(deadline) {
			break
		}

		r.log.Warn("tts synthesis retry",
			"provider", r.name,
			"attempt", attempt+1,
			"category", string(cat),
			"delay", d,
			"error", err,
		)

		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			// The context error is the actual reason the loop stopped, and
			// it categorizes as "canceled" — more useful to the caller than
			// the upstream failure that triggered the backoff.
			return nil, ctx.Err()
		case <-t.C:
		}
	}

	return nil, lastErr
}
