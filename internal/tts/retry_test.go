package tts

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"testing"
)

// fakeTimeout is a net.Error reporting a timeout, standing in for the
// transport errors http.Client.Do surfaces on a stalled connection.
type fakeTimeout struct{}

func (fakeTimeout) Error() string { return "i/o timeout" }
func (fakeTimeout) Timeout() bool { return true }
func (fakeTimeout) Temporary() bool {
	return true
}

var _ net.Error = fakeTimeout{}

func TestCategorize(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want Category
	}{
		{"nil", nil, ""},
		{"rate limited", errors.New("elevenlabs: status 429: rate limited"), CategoryRateLimited},
		{"service unavailable", errors.New("deepgram: status 503: down"), CategoryServiceUnavailable},
		{
			// The azure error embeds the response body after the real status.
			// A categorizer taking the LAST match would read 503 out of the
			// body and retry a rejected credential.
			"azure auth with a status token inside the body",
			fmt.Errorf("azure: status 401: body=%q ms-error-code=%q ms-error-message=%q",
				`{"detail":"status 503 upstream"}`, "", ""),
			CategoryPermanentAuth,
		},
		{"bad request", errors.New("elevenlabs: status 400: bad"), CategoryPermanentInput},
		{"unprocessable", errors.New("elevenlabs: status 422: unprocessable"), CategoryPermanentInput},
		{"not found", errors.New("elevenlabs: status 404: voice not found"), CategoryPermanentInput},
		{"request timeout", errors.New("deepgram: status 408: too slow"), CategoryRetryable},
		{"unmapped status", errors.New("elevenlabs: status 418: teapot"), CategoryUnknown},
		{
			"cancelled through url.Error",
			fmt.Errorf("elevenlabs request: %w", &url.Error{
				Op: "Post", URL: "http://x", Err: context.Canceled,
			}),
			CategoryCanceled,
		},
		{"deadline exceeded", fmt.Errorf("azure request: %w", context.DeadlineExceeded), CategoryRetryable},
		{
			"transport timeout",
			fmt.Errorf("deepgram request: %w", &url.Error{Op: "Post", URL: "http://x", Err: fakeTimeout{}}),
			CategoryRetryable,
		},
		{
			"transport refused",
			fmt.Errorf("elevenlabs request: %w", &url.Error{
				Op: "Post", URL: "http://x", Err: errors.New("connection refused"),
			}),
			CategoryRetryable,
		},
		{"polly sdk error", fmt.Errorf("polly: synthesize: %w", errors.New("AccessDenied")), CategoryUnknown},
		{"missing api key", errors.New("elevenlabs: no API key provided"), CategoryUnknown},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Categorize(tc.err); got != tc.want {
				t.Fatalf("Categorize(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestCategoryRetryable pins the retryable set, including the deliberate
// divergence from ai-runtime: unknown is NOT retryable.
func TestCategoryRetryable(t *testing.T) {
	retryable := []Category{CategoryRetryable, CategoryRateLimited, CategoryServiceUnavailable}
	terminal := []Category{
		CategoryPermanentAuth, CategoryPermanentInput, CategoryCanceled,
		CategoryUnknown, CategoryPlayback, Category(""),
	}
	for _, c := range retryable {
		if !c.retryable() {
			t.Errorf("%q.retryable() = false, want true", c)
		}
	}
	for _, c := range terminal {
		if c.retryable() {
			t.Errorf("%q.retryable() = true, want false", c)
		}
	}
}
