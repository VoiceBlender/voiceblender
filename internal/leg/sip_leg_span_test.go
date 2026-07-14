package leg

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel/attribute"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// newTestTracer returns a hermetic tracer plus its in-memory exporter. No
// process global is touched, so these tests are safe in parallel and under
// -race.
func newTestTracer(t *testing.T) (trace.Tracer, *tracetest.InMemoryExporter) {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return tp.Tracer("test"), exp
}

// TestSIPLegEndRootSpanExactlyOnce is criterion 2's guard. Concurrent
// terminal paths race to end the same leg's span; exactly one span must be
// exported. "At least once" is not the property — a double End would be
// swallowed by the SDK, so only counting exports catches it.
func TestSIPLegEndRootSpanExactlyOnce(t *testing.T) {
	tracer, exp := newTestTracer(t)

	l := &SIPLeg{id: "leg-1", legType: TypeSIPOutbound}
	l.rootSpan = startRootSpan(context.Background(), tracer, l.legType, l.id)

	const goroutines = 32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			l.EndRootSpan("remote_bye")
		}()
	}
	close(start)
	wg.Wait()

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("exported %d spans after %d concurrent EndRootSpan calls, want exactly 1", len(spans), goroutines)
	}
	if spans[0].Name != "sip.leg" {
		t.Errorf("span name = %q, want sip.leg", spans[0].Name)
	}
}

// TestSIPLegEndRootSpanSequentialCallsExportOnce covers the non-concurrent
// double-end, which is the shape a second terminal path would actually take.
func TestSIPLegEndRootSpanSequentialCallsExportOnce(t *testing.T) {
	tracer, exp := newTestTracer(t)

	l := &SIPLeg{id: "leg-1", legType: TypeSIPOutbound}
	l.rootSpan = startRootSpan(context.Background(), tracer, l.legType, l.id)

	l.EndRootSpan("api_hangup")
	l.EndRootSpan("remote_bye")
	l.EndRootSpan("shutdown")

	if spans := exp.GetSpans(); len(spans) != 1 {
		t.Fatalf("exported %d spans after 3 EndRootSpan calls, want exactly 1", len(spans))
	}

	// The first reason wins; later ones must not overwrite it.
	attrs := spanAttrs(t, exp)
	if got := attrs["leg.disconnect_reason"]; got != "api_hangup" {
		t.Errorf("leg.disconnect_reason = %q, want api_hangup (the first reason)", got)
	}
}

// TestEndRootSpanNilSpanNoPanic — trace.Span is an interface, so an unset
// field is a nil interface and End() on it panics. The recover sites in
// sip_leg.go cover the media loops only, so a panic on a teardown path would
// be unrecovered and kill the media server.
func TestEndRootSpanNilSpanNoPanic(t *testing.T) {
	l := &SIPLeg{id: "leg-nospan", legType: TypeSIPOutbound}
	l.EndRootSpan("api_hangup") // must not panic
	if got := l.RootSpan(); got == nil {
		t.Error("RootSpan() = nil, want a noop span — callers must never get a nil span")
	}
}

// TestStartRootSpanNilTracerNoPanic — a caller that has not wired tracing
// must get a usable noop span, not a nil one.
func TestStartRootSpanNilTracerNoPanic(t *testing.T) {
	l := &SIPLeg{id: "leg-x", legType: TypeSIPOutbound}
	l.rootSpan = startRootSpan(context.Background(), nil, l.legType, l.id)
	if l.rootSpan == nil {
		t.Fatal("startRootSpan(nil tracer) = nil, want a noop span")
	}
	l.EndRootSpan("api_hangup") // must not panic
}

// TestRootSpanIsRoot — the leg span must have no parent, or it would be
// swallowed into an unrelated trace instead of being the leg's own root.
func TestRootSpanIsRoot(t *testing.T) {
	tracer, exp := newTestTracer(t)
	l := &SIPLeg{id: "leg-1", legType: TypeSIPOutbound}
	l.rootSpan = startRootSpan(context.Background(), tracer, l.legType, l.id)
	l.EndRootSpan("remote_bye")

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("exported %d spans, want 1", len(spans))
	}
	if spans[0].Parent.IsValid() {
		t.Errorf("leg span has parent %v, want none (it is a root span)", spans[0].Parent)
	}
}

func TestEndRootSpanStatus(t *testing.T) {
	tests := []struct {
		reason  string
		isError bool
	}{
		{"api_hangup", false},
		{"remote_bye", false},
		{"caller_cancel", false},
		{"max_duration", false},
		{"room_deleted", false},
		{"transfer_completed", false},
		{"shutdown", false},
		{"cancelled", false},
		{"invite_failed", true},
		{"ring_timeout", true},
		{"rtp_timeout", true},
		{"session_expired", true},
		{"panic", true},
		{"busy", true},
		{"declined", true},
		// Synthesized reasons an allowlist of failures could never enumerate.
		{"sip_503", true},
		{"ice_failure", true},
		{"ice_disconnected", true},
	}
	for _, tc := range tests {
		t.Run(tc.reason, func(t *testing.T) {
			tracer, exp := newTestTracer(t)
			l := &SIPLeg{id: "leg-1", legType: TypeSIPOutbound}
			l.rootSpan = startRootSpan(context.Background(), tracer, l.legType, l.id)
			l.EndRootSpan(tc.reason)

			spans := exp.GetSpans()
			if len(spans) != 1 {
				t.Fatalf("exported %d spans, want 1", len(spans))
			}
			gotErr := spans[0].Status.Code == codes.Error
			if gotErr != tc.isError {
				t.Errorf("reason %q: error status = %v, want %v", tc.reason, gotErr, tc.isError)
			}
			if got := spanAttrs(t, exp)["leg.disconnect_reason"]; got != tc.reason {
				t.Errorf("leg.disconnect_reason = %q, want %q", got, tc.reason)
			}
		})
	}
}

// TestSIPLegSatisfiesRootSpanEnder pins the optional interface SIP legs must
// satisfy for the terminal paths to find them.
func TestSIPLegSatisfiesRootSpanEnder(t *testing.T) {
	var _ RootSpanEnder = (*SIPLeg)(nil)
}

// TestNoopSpanIsNotNil documents the assumption RootSpan() relies on.
func TestNoopSpanIsNotNil(t *testing.T) {
	var s trace.Span = noop.Span{}
	if s == nil {
		t.Fatal("noop.Span{} boxed to a nil trace.Span")
	}
	s.End()
}

func spanAttrs(t *testing.T, exp *tracetest.InMemoryExporter) map[string]string {
	t.Helper()
	spans := exp.GetSpans()
	if len(spans) == 0 {
		t.Fatal("no spans exported")
	}
	out := make(map[string]string)
	for _, kv := range spans[0].Attributes {
		out[string(kv.Key)] = kv.Value.Emit()
	}
	return out
}

// countingSpan counts the calls EndRootSpan makes on the span it holds.
//
// This is what actually proves the spanEndOnce claim. Counting exported spans
// cannot: the SDK's recordingSpan.End checks isRecording() under its own lock
// and silently returns on a second call, so the exporter sees exactly one span
// whether or not our Once is there. Exactly-once has to be asserted where it
// is our property — at the call boundary.
type countingSpan struct {
	noop.Span
	ends  atomic.Int32
	attrs atomic.Int32
	stats atomic.Int32
}

func (s *countingSpan) End(...trace.SpanEndOption)          { s.ends.Add(1) }
func (s *countingSpan) SetAttributes(...attribute.KeyValue) { s.attrs.Add(1) }
func (s *countingSpan) SetStatus(codes.Code, string)        { s.stats.Add(1) }

// TestEndRootSpanCallsEndExactlyOnceConcurrent is criterion 2's real
// exactly-once guard. It goes RED if spanEndOnce is removed or defeated.
func TestEndRootSpanCallsEndExactlyOnceConcurrent(t *testing.T) {
	span := &countingSpan{}
	l := &SIPLeg{id: "leg-1", legType: TypeSIPOutbound, rootSpan: span}

	const goroutines = 32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			l.EndRootSpan("remote_bye")
		}()
	}
	close(start)
	wg.Wait()

	if got := span.ends.Load(); got != 1 {
		t.Errorf("End() called %d times across %d concurrent EndRootSpan calls, want exactly 1", got, goroutines)
	}
	if got := span.attrs.Load(); got != 1 {
		t.Errorf("SetAttributes called %d times, want exactly 1 — no mutation after the span is ended", got)
	}
	if got := span.stats.Load(); got != 1 {
		t.Errorf("SetStatus called %d times, want exactly 1", got)
	}
}

// TestEndRootSpanCallsEndExactlyOnceSequential is the same contract for the
// shape a second terminal path actually takes: two ordered calls.
func TestEndRootSpanCallsEndExactlyOnceSequential(t *testing.T) {
	span := &countingSpan{}
	l := &SIPLeg{id: "leg-1", legType: TypeSIPOutbound, rootSpan: span}

	l.EndRootSpan("api_hangup")
	l.EndRootSpan("remote_bye")
	l.EndRootSpan("shutdown")

	if got := span.ends.Load(); got != 1 {
		t.Errorf("End() called %d times across 3 EndRootSpan calls, want exactly 1", got)
	}
}
