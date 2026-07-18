package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/leg"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// newSpanTestTracer returns a hermetic tracer plus its in-memory exporter.
// No process global is touched, so these tests are safe under -race.
func newSpanTestTracer(t *testing.T) (trace.Tracer, *tracetest.InMemoryExporter) {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return tp.Tracer("test"), exp
}

// spanTestEngine builds a real engine. NewEngine binds no socket (only Serve
// does), so this needs no network and cannot collide with other tests.
func spanTestEngine(t *testing.T) *sipmod.Engine {
	t.Helper()
	eng, err := sipmod.NewEngine(sipmod.EngineConfig{
		BindIP:   "127.0.0.1",
		BindPort: 5060,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return eng
}

// spanEnderLeg is an apiMockLeg that records the reasons EndRootSpan is
// called with. apiMockLeg deliberately does not implement leg.RootSpanEnder
// (only SIP legs carry a span), so publishDisconnect's type assertion skips
// it silently — hence the wrapper.
//
// The assertion is at the CALL boundary rather than through a span exporter
// on purpose: the real SIPLeg's spanEndOnce swallows every call after the
// first, so an exporter-based test could not tell which reason won the race
// — the same reason the countingSpan tests in internal/leg assert here.
type spanEnderLeg struct {
	*apiMockLeg
	mu      sync.Mutex
	reasons []string
}

func (l *spanEnderLeg) EndRootSpan(reason string) {
	l.mu.Lock()
	l.reasons = append(l.reasons, reason)
	l.mu.Unlock()
}

func (l *spanEnderLeg) recorded() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.reasons...)
}

func newSpanEnderLeg(id string) *spanEnderLeg {
	return &spanEnderLeg{apiMockLeg: &apiMockLeg{id: id, createdAt: time.Now()}}
}

// TestPublishDisconnectEndsRootSpan pins the span-close funnel every
// API-driven disconnect flows through. Deleting the RootSpanEnder block from
// publishDisconnect leaves every SIP leg's span unended and unexported —
// the feature's headline claim silently becomes zero spans on the normal
// path.
func TestPublishDisconnectEndsRootSpan(t *testing.T) {
	var _ leg.RootSpanEnder = (*spanEnderLeg)(nil)

	s := newTestServer(t)
	l := newSpanEnderLeg("leg-1")

	s.publishDisconnect(l, "api_hangup")

	got := l.recorded()
	if len(got) != 1 || got[0] != "api_hangup" {
		t.Fatalf("EndRootSpan reasons = %v, want [api_hangup]", got)
	}
}

// TestPublishDisconnectSpanReasonIsClaimWinner is the invariant API.md
// documents: the span's leg.disconnect_reason is "the same reason string as
// the leg.disconnected event". Only the ClaimDisconnect winner publishes an
// event, so only the winner may stamp the span. A racing loser (DELETE
// /legs/{id} against the RTP-timeout callback is the real shape) must not
// overwrite the reason with one no event ever carried.
func TestPublishDisconnectSpanReasonIsClaimWinner(t *testing.T) {
	s := newTestServer(t)
	l := newSpanEnderLeg("leg-1")

	s.publishDisconnect(l, "api_hangup")  // wins the CAS
	s.publishDisconnect(l, "rtp_timeout") // loses; must not touch the span

	got := l.recorded()
	if len(got) != 1 || got[0] != "api_hangup" {
		t.Fatalf("EndRootSpan reasons = %v, want [api_hangup] — the span's reason must be the ClaimDisconnect winner's, matching the leg.disconnected event", got)
	}
}

// TestCreateSIPOutboundLegBadAMDParamsEndsRootSpan covers a terminal path any
// client can trigger at will: the constructor opens the span, the AMD reject
// returns 400 before LegMgr.Add, so nothing can ever reach the leg again —
// no publishDisconnect, no Hangup, and the shutdown sweep cannot see it.
// Without an explicit end here the span is simply never exported.
func TestCreateSIPOutboundLegBadAMDParamsEndsRootSpan(t *testing.T) {
	s := newTestServer(t)
	tracer, exp := newSpanTestTracer(t)
	s.Tracer = tracer
	s.SIPEngine = spanTestEngine(t)

	// A greeting window longer than the whole analysis window can never be
	// reached, so Validate rejects it.
	_, err := s.doCreateSIPOutboundLeg(CreateLegRequest{
		Type: "sip",
		To:   "sip:bob@127.0.0.1:5060",
		AMD:  &AMDParams{TotalAnalysisTime: 1000, GreetingDuration: 5000},
	})

	var apiErr *apiError
	if !errors.As(err, &apiErr) || apiErr.Code != http.StatusBadRequest {
		t.Fatalf("doCreateSIPOutboundLeg error = %v, want a 400 apiError", err)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("exported %d spans, want 1 — the rejected leg's root span must be ended, or any client can mint unended spans by POSTing bad AMD params", len(spans))
	}
	attrs := map[string]string{}
	for _, kv := range spans[0].Attributes {
		attrs[string(kv.Key)] = kv.Value.Emit()
	}
	if got := attrs["leg.disconnect_reason"]; got != "bad_amd_params" {
		t.Errorf("leg.disconnect_reason = %q, want bad_amd_params", got)
	}
	if spans[0].Status.Code != codes.Error {
		t.Errorf("span status = %v, want Error — a rejected leg did not end normally", spans[0].Status.Code)
	}
}
