package api

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
	"go.opentelemetry.io/otel/codes"
)

// TestWatchLegDialogEndDisconnectsOnRemoteBye pins the monitor a
// REFER-originated leg depends on. Once the transfer completes the referrer is
// torn down and nothing else observes the transferred leg's dialog: the engine
// only drops the dialog from its cache on BYE, so without this monitor the
// remote's BYE produces no leg.disconnected at all and the leg's root span
// stays open until shutdown ends it with a reason that never happened.
func TestWatchLegDialogEndDisconnectsOnRemoteBye(t *testing.T) {
	s := newTestServer(t)
	tracer, exp := newSpanTestTracer(t)
	s.Tracer = tracer
	s.SIPEngine = spanTestEngine(t)

	l := leg.NewSIPOutboundPendingLeg(s.SIPEngine, nil, s.Tracer, s.Log)
	s.LegMgr.Add(l)

	got := make(chan events.Event, 4)
	unsub := s.Bus.Subscribe(func(e events.Event) {
		if e.Type == events.LegDisconnected {
			got <- e
		}
	})
	t.Cleanup(unsub)

	// The dialog context is what sipgo cancels when the remote sends BYE.
	dialogCtx, remoteBye := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.watchLegDialogEnd(l, dialogCtx)
	}()

	remoteBye()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchLegDialogEnd did not return after the dialog ended")
	}

	select {
	case e := <-got:
		data, ok := e.Data.(*events.LegDisconnectedData)
		if !ok {
			t.Fatalf("leg.disconnected data = %T, want *events.LegDisconnectedData", e.Data)
		}
		if data.LegID != l.ID() {
			t.Errorf("leg_id = %q, want %q", data.LegID, l.ID())
		}
		if data.CDR.Reason != "remote_bye" {
			t.Errorf("cdr.reason = %q, want remote_bye", data.CDR.Reason)
		}
	default:
		t.Fatal("no leg.disconnected published — a remote BYE on a transferred leg must end the leg")
	}

	if _, ok := s.LegMgr.Get(l.ID()); ok {
		t.Error("leg still registered after remote BYE — cleanupLeg must remove it")
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("exported %d spans, want 1 — the transferred leg's root span must be ended on remote BYE", len(spans))
	}
	attrs := map[string]string{}
	for _, kv := range spans[0].Attributes {
		attrs[string(kv.Key)] = kv.Value.Emit()
	}
	if got := attrs["leg.disconnect_reason"]; got != "remote_bye" {
		t.Errorf("leg.disconnect_reason = %q, want remote_bye", got)
	}
	if spans[0].Status.Code == codes.Unset {
		t.Errorf("span status is Unset — the span was never ended with a reason")
	}
}

// TestWatchLegDialogEndExitsOnLocalTeardown pins the local-teardown exit. A leg
// torn down through the API (or by the RTP-timeout hook) hangs up locally and
// sends a BYE, but a vanished peer may never return the 200 that ends the
// sipgo dialog — so the dialog context never fires. The monitor must still
// exit off the leg's own cancelled context, and must not publish a second,
// contradictory disconnect on top of the one the local teardown already
// claimed. With the dialog context wired as the only wake-up, the monitor
// blocks for the process lifetime and this test fails.
func TestWatchLegDialogEndExitsOnLocalTeardown(t *testing.T) {
	s := newTestServer(t)
	tracer, exp := newSpanTestTracer(t)
	s.Tracer = tracer
	s.SIPEngine = spanTestEngine(t)

	l := leg.NewSIPOutboundPendingLeg(s.SIPEngine, nil, s.Tracer, s.Log)
	s.LegMgr.Add(l)

	// Local teardown wins first, exactly as DELETE /legs/{id} would: cleanupLeg
	// hangs the leg up (cancelling its context) and publishDisconnect claims
	// the sole disconnect.
	s.cleanupLeg(l)
	s.publishDisconnect(l, "api_hangup")

	var extra int
	unsub := s.Bus.Subscribe(func(e events.Event) {
		if e.Type == events.LegDisconnected {
			extra++
		}
	})
	t.Cleanup(unsub)

	// The peer never answers our BYE, so the dialog context never ends.
	dialogCtx := context.Background()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.watchLegDialogEnd(l, dialogCtx)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchLegDialogEnd did not exit after local teardown — it leaks when the peer never answers the BYE")
	}

	if extra != 0 {
		t.Errorf("watchLegDialogEnd published %d extra leg.disconnected events, want 0", extra)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("exported %d spans, want 1", len(spans))
	}
	attrs := map[string]string{}
	for _, kv := range spans[0].Attributes {
		attrs[string(kv.Key)] = kv.Value.Emit()
	}
	if got := attrs["leg.disconnect_reason"]; got != "api_hangup" {
		t.Errorf("leg.disconnect_reason = %q, want api_hangup — the first teardown owns the reason", got)
	}
}

// TestOriginateForReferWatchesDialogAfterConnect covers the wiring the unit
// tests above cannot reach: that originateForRefer actually installs the
// monitor, and that it does so only after leg.connected is published. Driving
// the real SIP INVITE would need a live peer, so the sequence is asserted on
// the source of originateForRefer instead — a call to watchLegDialogEnd must
// be present, must follow the LegConnected publish, and must not be spawned in
// a goroutine (which would let leg.disconnected race ahead of leg.connected for
// a call that ends immediately).
func TestOriginateForReferWatchesDialogAfterConnect(t *testing.T) {
	body := funcBody(t, "transfer.go", "originateForRefer")

	watch := strings.Index(body, "s.watchLegDialogEnd(newLeg,")
	if watch < 0 {
		t.Fatal("originateForRefer does not call watchLegDialogEnd — a transferred leg is left unwatched, so a remote BYE publishes nothing and its span never ends")
	}

	connected := strings.Index(body, "events.LegConnected")
	if connected < 0 {
		t.Fatal("originateForRefer no longer publishes events.LegConnected")
	}
	if watch < connected {
		t.Error("watchLegDialogEnd is wired before leg.connected is published — leg.disconnected could be published first")
	}

	if strings.Contains(body, "go s.watchLegDialogEnd(") {
		t.Error("watchLegDialogEnd is spawned in a goroutine — it must block in originateForRefer's own goroutine so leg.connected always precedes leg.disconnected")
	}
}

// funcBody returns the source text of the named func's body in path, which is
// relative to this package's directory.
func funcBody(t *testing.T, path, name string) string {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), path, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == name && fn.Body != nil {
			return string(src[fn.Body.Pos()-1 : fn.Body.End()-1])
		}
	}
	t.Fatalf("func %s not found in %s", name, path)
	return ""
}
