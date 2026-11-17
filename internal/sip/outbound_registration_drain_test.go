package sip

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// drainRegistrar answers every REGISTER with 200 and records its Expires.
type drainRegistrar struct {
	mu      sync.Mutex
	expires []string
	port    int
}

func (f *drainRegistrar) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.expires...)
}

func startDrainRegistrar(t *testing.T) *drainRegistrar {
	t.Helper()
	f := &drainRegistrar{port: pickFreePort(t, "udp")}
	ua, err := sipgo.NewUA(sipgo.WithUserAgent("fake-registrar"))
	if err != nil {
		t.Fatalf("NewUA: %v", err)
	}
	srv, err := sipgo.NewServer(ua)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.OnRegister(func(req *sip.Request, tx sip.ServerTransaction) {
		exp := ""
		if h := req.GetHeader("Expires"); h != nil {
			exp = h.Value()
		}
		f.mu.Lock()
		f.expires = append(f.expires, exp)
		f.mu.Unlock()
		res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
		if exp != "" {
			res.AppendHeader(sip.NewHeader("Expires", exp))
		}
		_ = tx.Respond(res)
	})
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	ctx2 := context.WithValue(ctx, sipgo.ListenReadyCtxKey, sipgo.ListenReadyCtxValue(ready))
	go srv.ListenAndServe(ctx2, "udp", fmt.Sprintf("127.0.0.1:%d", f.port))
	t.Cleanup(func() { cancel(); ua.Close() })
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("fake registrar did not start")
	}
	return f
}

func startServingEngine(t *testing.T) *Engine {
	t.Helper()
	engine, err := NewEngine(EngineConfig{
		BindIP:   "127.0.0.1",
		BindPort: pickFreePort(t, "udp"),
		SIPHost:  "test-vb",
		Codecs:   []codec.CodecType{codec.CodecPCMU},
		Log:      slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	listening := make(chan struct{})
	engine.OnListening(func() { close(listening) })
	ctx, cancel := context.WithCancel(context.Background())
	go engine.Serve(ctx)
	t.Cleanup(cancel)
	select {
	case <-listening:
	case <-time.After(3 * time.Second):
		t.Fatal("engine did not start")
	}
	return engine
}

func waitTrunkStatus(t *testing.T, tr Trunk, want TrunkStatus) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if tr.Snapshot().Status == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("trunk status = %s, want %s", tr.Snapshot().Status, want)
}

func newDrainTrunk(engine *Engine, bus *events.Bus, port int, id string, unregister bool) *OutboundRegistration {
	return NewOutboundRegistration(engine, bus, nil, OutboundRegistrationConfig{}, OutboundRegistrationParams{
		ID:                id,
		RegistrarURI:      sip.Uri{Scheme: "sip", Host: "127.0.0.1", Port: port},
		AOR:               sip.Uri{Scheme: "sip", User: id, Host: "127.0.0.1"},
		UnregisterOnDrain: unregister,
	})
}

func TestTrunkManager_PauseAndResumeForDrain(t *testing.T) {
	reg := startDrainRegistrar(t)
	engine := startServingEngine(t)
	bus := events.NewBus("test")
	var mu sync.Mutex
	var reasons []string
	bus.Subscribe(func(e events.Event) {
		if d, ok := e.Data.(*events.SIPOutboundRegistrationExpiredData); ok {
			mu.Lock()
			reasons = append(reasons, d.TrunkID+":"+d.Reason)
			mu.Unlock()
		}
	})

	mgr := engine.Trunks()
	flagged := newDrainTrunk(engine, bus, reg.port, "flagged", true)
	kept := newDrainTrunk(engine, bus, reg.port, "kept", false)
	mgr.Add(flagged)
	mgr.Add(kept)
	mgr.StartTrunk(flagged)
	mgr.StartTrunk(kept)
	waitTrunkStatus(t, flagged, TrunkStatusActive)
	waitTrunkStatus(t, kept, TrunkStatusActive)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	mgr.PauseForDrain(ctx)
	if flagged.Snapshot().Status != TrunkStatusExpired {
		t.Fatalf("flagged trunk status = %s, want expired", flagged.Snapshot().Status)
	}
	if kept.Snapshot().Status != TrunkStatusActive {
		t.Fatalf("unflagged trunk status = %s, want active", kept.Snapshot().Status)
	}
	unregisters := 0
	for _, e := range reg.seen() {
		if e == "0" {
			unregisters++
		}
	}
	if unregisters != 1 {
		t.Fatalf("Expires values = %v, want exactly one unregister", reg.seen())
	}

	late := newDrainTrunk(engine, bus, reg.port, "late", true)
	mgr.Add(late)
	mgr.StartTrunk(late)
	time.Sleep(100 * time.Millisecond)
	if late.Snapshot().Status != TrunkStatusExpired {
		t.Fatalf("trunk created during drain status = %s, want expired", late.Snapshot().Status)
	}

	mgr.ResumeAfterDrain()
	waitTrunkStatus(t, flagged, TrunkStatusActive)
	waitTrunkStatus(t, late, TrunkStatusActive)

	mu.Lock()
	defer mu.Unlock()
	if len(reasons) != 1 || reasons[0] != "flagged:drain" {
		t.Fatalf("expired events = %v, want [flagged:drain]", reasons)
	}
}

func TestOutboundRegistration_StopAfterPauseSkipsSecondUnregister(t *testing.T) {
	reg := startDrainRegistrar(t)
	engine := startServingEngine(t)
	tr := newDrainTrunk(engine, nil, reg.port, "t", true)
	engine.Trunks().Add(tr)
	engine.Trunks().StartTrunk(tr)
	waitTrunkStatus(t, tr, TrunkStatusActive)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := tr.Pause(ctx); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	before := len(reg.seen())
	if err := tr.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if after := len(reg.seen()); after != before {
		t.Fatalf("Stop after Pause sent %d more REGISTERs", after-before)
	}
	tr.Resume()
	time.Sleep(100 * time.Millisecond)
	if tr.Snapshot().Status != TrunkStatusExpired {
		t.Fatalf("Resume after Stop changed status to %s", tr.Snapshot().Status)
	}
}

func TestOutboundRegistration_PauseKeepsTLSTrust(t *testing.T) {
	engine, err := NewEngine(EngineConfig{
		BindIP:   "127.0.0.1",
		BindPort: pickFreePort(t, "udp"),
		SIPHost:  "test",
		Log:      slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	tr := NewOutboundRegistration(engine, nil, nil, OutboundRegistrationConfig{}, OutboundRegistrationParams{
		ID:                    "tls",
		RegistrarURI:          sip.Uri{Scheme: "sips", Host: "registrar.invalid", Port: 5061},
		AOR:                   sip.Uri{Scheme: "sips", User: "alice", Host: "registrar.invalid"},
		TLSInsecureSkipVerify: true,
		UnregisterOnDrain:     true,
	})
	if !engine.peerTrust.trusted("registrar.invalid") {
		t.Fatal("trust not applied at construction")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_ = tr.Pause(ctx)
	if !engine.peerTrust.trusted("registrar.invalid") {
		t.Fatal("Pause removed the TLS exemption")
	}
	_ = tr.Stop(ctx)
	if engine.peerTrust.trusted("registrar.invalid") {
		t.Fatal("Stop kept the TLS exemption")
	}
}
