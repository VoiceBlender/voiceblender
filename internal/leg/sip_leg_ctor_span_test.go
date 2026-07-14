package leg

import (
	"io"
	"log/slog"
	"testing"

	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"go.opentelemetry.io/otel/codes"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// testEngine builds a real engine. NewEngine binds no socket (Serve does), so
// this is cheap and needs no network.
func testEngine(t *testing.T) *sipmod.Engine {
	t.Helper()
	eng, err := sipmod.NewEngine(sipmod.EngineConfig{
		BindIP:   "127.0.0.1",
		BindPort: 5060,
		Log:      discardLog(),
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return eng
}

// testInboundCall builds an InboundCall far enough for NewSIPInboundLeg.
func testInboundCall(t *testing.T) *sipmod.InboundCall {
	t.Helper()
	req := sip.NewRequest(sip.INVITE, sip.Uri{User: "bob", Host: "example.com"})
	req.AppendHeader(sip.NewHeader("X-Tenant", "acme"))
	req.AppendHeader(sip.NewHeader("Call-ID", "call-abc"))
	ds := &sipgo.DialogServerSession{}
	ds.InviteRequest = req
	ds.Init()
	return &sipmod.InboundCall{Dialog: ds, Request: req}
}

// TestNewSIPInboundLegStartsRootSpan is criterion 2's inbound guard. It drives
// the real constructor, so it goes RED if the span-start is removed from
// NewSIPInboundLeg — which is the whole point: an inbound leg with no span
// would nil-panic or silently export nothing on every teardown.
func TestNewSIPInboundLegStartsRootSpan(t *testing.T) {
	tracer, exp := newTestTracer(t)

	l := NewSIPInboundLeg(testInboundCall(t), testEngine(t), tracer, discardLog())

	if l.rootSpan == nil {
		t.Fatal("NewSIPInboundLeg left rootSpan nil — the inbound constructor must start the leg's root span")
	}
	if !l.rootSpan.SpanContext().IsValid() {
		t.Fatal("NewSIPInboundLeg produced an invalid (noop) span context; want a recording span from the injected tracer")
	}

	l.EndRootSpan("remote_bye")

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("exported %d spans for an inbound leg, want exactly 1", len(spans))
	}
	attrs := spanAttrs(t, exp)
	if got := attrs["leg.type"]; got != string(TypeSIPInbound) {
		t.Errorf("leg.type = %q, want %q", got, TypeSIPInbound)
	}
	if got := attrs["leg.id"]; got != l.ID() {
		t.Errorf("leg.id = %q, want %q", got, l.ID())
	}
}

// TestNewSIPOutboundPendingLegStartsRootSpan — the pending leg is the
// longest-lived span (it exists before the INVITE is even sent), so it is
// where an unended span would show up first.
func TestNewSIPOutboundPendingLegStartsRootSpan(t *testing.T) {
	tracer, exp := newTestTracer(t)

	l := NewSIPOutboundPendingLeg(testEngine(t), nil, tracer, discardLog())

	if l.rootSpan == nil {
		t.Fatal("NewSIPOutboundPendingLeg left rootSpan nil — the pending constructor must start the leg's root span")
	}
	if !l.rootSpan.SpanContext().IsValid() {
		t.Fatal("NewSIPOutboundPendingLeg produced an invalid (noop) span context")
	}

	l.EndRootSpan("ring_timeout")

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("exported %d spans, want exactly 1", len(spans))
	}
	if got := spanAttrs(t, exp)["leg.type"]; got != string(TypeSIPOutbound) {
		t.Errorf("leg.type = %q, want %q", got, TypeSIPOutbound)
	}
}

// TestNewSIPOutboundLegNoCommonCodecStartsRootSpan covers the third
// constructor via its early return. The plan flagged this path as a possible
// span leak: the span is started before the return, so the leg comes back
// unusable but with a live span. It is not a leak — the caller's teardown
// ends it — and this pins that the span exists and is endable.
func TestNewSIPOutboundLegNoCommonCodecStartsRootSpan(t *testing.T) {
	tracer, exp := newTestTracer(t)

	dc := &sipgo.DialogClientSession{}
	dc.InviteRequest = sip.NewRequest(sip.INVITE, sip.Uri{User: "bob", Host: "example.com"})
	dc.Init()
	// An answer offering no codec we support forces the early return.
	call := &sipmod.OutboundCall{Dialog: dc, RemoteSDP: &sipmod.SDPMedia{}}

	l := NewSIPOutboundLeg(call, testEngine(t), tracer, discardLog())

	if l.rootSpan == nil {
		t.Fatal("NewSIPOutboundLeg left rootSpan nil on the no-common-codec early return")
	}
	l.EndRootSpan("no_common_codec")

	if spans := exp.GetSpans(); len(spans) != 1 {
		t.Fatalf("exported %d spans, want exactly 1", len(spans))
	}
}

// TestRecoverLoopAndHangupEndsRootSpan covers the panic-recovery path — the
// second leg-terminating path that publishes no disconnect, so nothing else
// would ever end its span.
func TestRecoverLoopAndHangupEndsRootSpan(t *testing.T) {
	tracer, exp := newTestTracer(t)

	l := NewSIPOutboundPendingLeg(testEngine(t), nil, tracer, discardLog())

	// Drive the recover site exactly as a panicking media loop does.
	func() {
		defer l.recoverLoopAndHangup("readLoop")
		panic("boom")
	}()

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("exported %d spans after a panic-recovery hangup, want exactly 1 — a panicked leg's span must not leak", len(spans))
	}
	if got := spanAttrs(t, exp)["leg.disconnect_reason"]; got != "panic" {
		t.Errorf("leg.disconnect_reason = %q, want panic", got)
	}
	if spans[0].Status.Code != codes.Error {
		t.Errorf("span status = %v, want Error for a panicked leg", spans[0].Status.Code)
	}
}
