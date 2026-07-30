package tts

import (
	"context"
	"errors"
	"net"
	"net/url"
	"regexp"
	"strconv"
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
