package api

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
)

// referTestEngine builds a bound SIP engine for tests that need to construct a
// real SIPLeg. newTestServer passes nil for the engine, and the outbound-leg
// constructor dereferences it.
func referTestEngine(t *testing.T) *sipmod.Engine {
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

// TestWatchLegDialogEndDisconnectsOnRemoteBye pins the monitor a
// REFER-originated leg depends on. Once the transfer completes the referrer is
// torn down and nothing else observes the transferred leg's dialog: the engine
// only drops the dialog from its cache on BYE, so without this monitor the
// remote's BYE produces no leg.disconnected at all and the leg stays in the
// manager for the process lifetime.
func TestWatchLegDialogEndDisconnectsOnRemoteBye(t *testing.T) {
	s := newTestServer(t)
	s.SIPEngine = referTestEngine(t)

	l := leg.NewSIPOutboundPendingLeg(s.SIPEngine, nil, s.Log)
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
		s.watchLegDialogEnd(l, dialogCtx, 0)
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
	s.SIPEngine = referTestEngine(t)

	l := leg.NewSIPOutboundPendingLeg(s.SIPEngine, nil, s.Log)
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
		s.watchLegDialogEnd(l, dialogCtx, 0)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchLegDialogEnd did not exit after local teardown — it leaks when the peer never answers the BYE")
	}

	if extra != 0 {
		t.Errorf("watchLegDialogEnd published %d extra leg.disconnected events, want 0", extra)
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
		t.Fatal("originateForRefer does not call watchLegDialogEnd — a transferred leg is left unwatched, so a remote BYE publishes nothing and the leg leaks")
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

// TestWatchLegDialogEndDisconnectsOnMaxDuration pins the max-duration cap that
// POST /v1/legs exposes as max_duration. Neither the dialog nor the leg context
// ends here, so only the timer can wake the monitor — and the disconnect must
// be attributed to the cap, not to a BYE that never arrived.
func TestWatchLegDialogEndDisconnectsOnMaxDuration(t *testing.T) {
	s := newTestServer(t)
	s.SIPEngine = referTestEngine(t)

	l := leg.NewSIPOutboundPendingLeg(s.SIPEngine, nil, s.Log)
	s.LegMgr.Add(l)

	got := make(chan events.Event, 4)
	unsub := s.Bus.Subscribe(func(e events.Event) {
		if e.Type == events.LegDisconnected {
			got <- e
		}
	})
	t.Cleanup(unsub)

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.watchLegDialogEnd(l, context.Background(), 10*time.Millisecond)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchLegDialogEnd did not return after max duration elapsed")
	}

	select {
	case e := <-got:
		data, ok := e.Data.(*events.LegDisconnectedData)
		if !ok {
			t.Fatalf("leg.disconnected data = %T, want *events.LegDisconnectedData", e.Data)
		}
		if data.CDR.Reason != "max_duration" {
			t.Errorf("cdr.reason = %q, want max_duration", data.CDR.Reason)
		}
	default:
		t.Fatal("no leg.disconnected published — a leg that outlives max_duration must be reaped")
	}

	if _, ok := s.LegMgr.Get(l.ID()); ok {
		t.Error("leg still registered after max duration — cleanupLeg must remove it")
	}
}

// TestWatchLegDialogEndNoMaxDurationDoesNotFire guards the zero value: a leg
// with no cap must not be reaped by a timer that fired immediately. Passing 0
// through as a live time.Timer would tear down every uncapped call at once.
func TestWatchLegDialogEndNoMaxDurationDoesNotFire(t *testing.T) {
	s := newTestServer(t)
	s.SIPEngine = referTestEngine(t)

	l := leg.NewSIPOutboundPendingLeg(s.SIPEngine, nil, s.Log)
	s.LegMgr.Add(l)

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.watchLegDialogEnd(l, context.Background(), 0)
	}()

	select {
	case <-done:
		t.Fatal("watchLegDialogEnd returned with no cap and a live dialog — an uncapped leg was reaped")
	case <-time.After(100 * time.Millisecond):
		// Still blocked, as it must be.
	}

	// Unblock so the goroutine does not outlive the test.
	l.Hangup(context.Background())
	<-done
}

// TestOriginateForReferWiresLegCallbacks pins the callback wiring a transferred
// leg shares with the other construction sites. setupLegEventForwarding carries
// DTMF, RTT and the RTP-timeout reaper; setupHoldCallbacks carries leg.hold and
// leg.unhold. Both must be installed before the leg is registered, or events
// arriving on a freshly-added leg have nowhere to go.
func TestOriginateForReferWiresLegCallbacks(t *testing.T) {
	body := funcBody(t, "transfer.go", "originateForRefer")

	for _, wiring := range []string{"s.setupLegEventForwarding(newLeg)", "s.setupHoldCallbacks(newLeg)"} {
		at := strings.Index(body, wiring)
		if at < 0 {
			t.Errorf("originateForRefer does not call %s — a transferred leg is wired differently from every other connected leg", wiring)
			continue
		}
		if add := strings.Index(body, "s.LegMgr.Add(newLeg)"); add >= 0 && at > add {
			t.Errorf("%s runs after the leg is registered — callbacks must be wired first", wiring)
		}
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
