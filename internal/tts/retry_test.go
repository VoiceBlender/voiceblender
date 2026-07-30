package tts

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"
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

// fakeProvider returns a scripted sequence of results, then repeats the last
// entry. It is only ever driven from a single goroutine — the decorator
// spawns none — so a plain counter is enough.
type fakeProvider struct {
	calls   int
	results []*Result
	errs    []error
}

func (f *fakeProvider) Synthesize(ctx context.Context, text string, opts Options) (*Result, error) {
	i := f.calls
	f.calls++
	if i >= len(f.errs) {
		i = len(f.errs) - 1
	}
	var res *Result
	if i < len(f.results) {
		res = f.results[i]
	}
	return res, f.errs[i]
}

// fastRetrying builds a decorator with the production attempt cap but
// millisecond delays, so the real timer and select code path runs without
// costing the suite half a second.
func fastRetrying(inner Provider) *retryingProvider {
	return &retryingProvider{
		inner:       inner,
		name:        "t",
		log:         slog.Default(),
		maxAttempts: ttsRetryMaxAttempts,
		base:        time.Millisecond,
		maxInterval: 2 * time.Millisecond,
		maxElapsed:  ttsRetryMaxElapsed,
	}
}

func TestRetrying_RetriesTransientThenSucceeds(t *testing.T) {
	want := &Result{Audio: io.NopCloser(strings.NewReader("pcm")), MimeType: "audio/pcm;rate=16000"}
	fake := &fakeProvider{
		results: []*Result{nil, nil, want},
		errs: []error{
			errors.New("elevenlabs: status 503: unavailable"),
			errors.New("elevenlabs: status 503: unavailable"),
			nil,
		},
	}
	r := fastRetrying(fake)

	got, err := r.Synthesize(context.Background(), "hi", Options{})
	if err != nil {
		t.Fatalf("Synthesize: unexpected error %v", err)
	}
	if fake.calls != 3 {
		t.Fatalf("inner calls = %d, want 3", fake.calls)
	}
	if got != want {
		t.Fatalf("Result = %p, want the inner Result %p returned by identity", got, want)
	}
}

var errFake401 = errors.New(`azure: status 401: body="invalid key"`)

func TestRetrying_PermanentAuthNotRetried(t *testing.T) {
	fake := &fakeProvider{errs: []error{errFake401}}
	r := fastRetrying(fake)

	_, err := r.Synthesize(context.Background(), "hi", Options{})
	if fake.calls != 1 {
		t.Fatalf("inner calls = %d, want 1 — a 401 must not be retried", fake.calls)
	}
	// errors.Is, not a substring match: a decorator that reformatted the
	// error with %v would still carry "401" in its message, so a substring
	// assertion could not tell wrapping from passthrough.
	if !errors.Is(err, errFake401) {
		t.Fatalf("error = %v, want the inner error returned by identity", err)
	}
}

func TestRetrying_ExhaustsAndReturnsLastError(t *testing.T) {
	// Retryable-shaped sentinels: an unclassifiable error is terminal, so
	// plain errors.New would stop the loop after one attempt and prove
	// nothing about the cap.
	err1 := errors.New("elevenlabs: status 503: down #1")
	err2 := errors.New("elevenlabs: status 503: down #2")
	err3 := errors.New("elevenlabs: status 503: down #3")
	err4 := errors.New("elevenlabs: status 503: down #4")
	fake := &fakeProvider{errs: []error{err1, err2, err3, err4}}
	r := fastRetrying(fake)

	_, err := r.Synthesize(context.Background(), "hi", Options{})
	if fake.calls != ttsRetryMaxAttempts {
		t.Fatalf("inner calls = %d, want %d", fake.calls, ttsRetryMaxAttempts)
	}
	if !errors.Is(err, err3) {
		t.Fatalf("error = %v, want the last attempt's error (%v)", err, err3)
	}
}

func TestRetrying_ContextCancelAbortsBackoff(t *testing.T) {
	fake := &fakeProvider{errs: []error{errors.New("elevenlabs: status 503: down")}}
	r := &retryingProvider{
		inner:       fake,
		name:        "t",
		log:         slog.Default(),
		maxAttempts: ttsRetryMaxAttempts,
		base:        2 * time.Second,
		maxInterval: 2 * time.Second,
		maxElapsed:  time.Minute,
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := r.Synthesize(ctx, "hi", Options{})
	elapsed := time.Since(start)

	// The elapsed bound is the assertion that fails fast: a backoff that
	// slept unconditionally would take seconds to trip the other two.
	if elapsed > 500*time.Millisecond {
		t.Fatalf("Synthesize took %v, want the backoff to abort on ctx cancel", elapsed)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if fake.calls != 1 {
		t.Fatalf("inner calls = %d, want 1 — no attempt after cancellation", fake.calls)
	}
}

func TestRetryDelay(t *testing.T) {
	// Attempt 10 is in the table on purpose: with ttsRetryMaxAttempts = 3 the
	// runtime only ever asks for attempts 0 and 1, where the ceiling never
	// binds, so a band assertion over the live attempts alone would stay
	// green with the clamp deleted.
	for _, attempt := range []int{0, 1, 3, 10} {
		want := ttsRetryBaseInterval
		for i := 0; i < attempt; i++ {
			want *= ttsRetryMultiplier
			if want > ttsRetryMaxInterval {
				want = ttsRetryMaxInterval
				break
			}
		}
		// The ±25% band is written out rather than derived from
		// ttsRetryJitterFraction: a band computed from the constant widens
		// with it, so changing the constant could not fail this test.
		lo := want * 3 / 4
		hi := want * 5 / 4

		seen := make(map[time.Duration]struct{})
		for i := 0; i < 200; i++ {
			d := retryDelay(attempt, ttsRetryBaseInterval, ttsRetryMaxInterval)
			if d < lo || d > hi {
				t.Fatalf("retryDelay(%d) = %v, want within [%v, %v]", attempt, d, lo, hi)
			}
			seen[d] = struct{}{}
		}
		if len(seen) < 2 {
			t.Fatalf("retryDelay(%d) produced %d distinct value(s) over 200 samples, want jitter", attempt, len(seen))
		}
	}
}

func TestRetrying_MaxElapsedStopsRetrying(t *testing.T) {
	sentinel := errors.New("elevenlabs: status 503: down")
	fake := &fakeProvider{errs: []error{sentinel}}
	r := &retryingProvider{
		inner:       fake,
		name:        "t",
		log:         slog.Default(),
		maxAttempts: ttsRetryMaxAttempts,
		base:        time.Second,
		maxInterval: time.Second,
		maxElapsed:  time.Nanosecond,
	}

	start := time.Now()
	_, err := r.Synthesize(context.Background(), "hi", Options{})
	elapsed := time.Since(start)

	if fake.calls != 1 {
		t.Fatalf("inner calls = %d, want 1 — the elapsed ceiling must stop the loop", fake.calls)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want %v", err, sentinel)
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("Synthesize took %v, want it to break out rather than sleep", elapsed)
	}
}

// TestNewRetryingUsesPolicyConstants guards the constructor: every other test
// builds the struct literal directly, so a wiring typo in NewRetrying would
// disable retry in production with the whole package still green.
func TestNewRetryingUsesPolicyConstants(t *testing.T) {
	fake := &fakeProvider{errs: []error{nil}}
	p := NewRetrying(fake, "t", slog.Default())

	r, ok := p.(*retryingProvider)
	if !ok {
		t.Fatalf("NewRetrying returned %T, want *retryingProvider", p)
	}
	if r.inner != Provider(fake) {
		t.Errorf("inner = %v, want the wrapped provider", r.inner)
	}
	if r.name != "t" {
		t.Errorf("name = %q, want %q", r.name, "t")
	}
	if r.maxAttempts != ttsRetryMaxAttempts {
		t.Errorf("maxAttempts = %d, want %d", r.maxAttempts, ttsRetryMaxAttempts)
	}
	if r.base != ttsRetryBaseInterval {
		t.Errorf("base = %v, want %v", r.base, ttsRetryBaseInterval)
	}
	if r.maxInterval != ttsRetryMaxInterval {
		t.Errorf("maxInterval = %v, want %v", r.maxInterval, ttsRetryMaxInterval)
	}
	if r.maxElapsed != ttsRetryMaxElapsed {
		t.Errorf("maxElapsed = %v, want %v", r.maxElapsed, ttsRetryMaxElapsed)
	}
}
