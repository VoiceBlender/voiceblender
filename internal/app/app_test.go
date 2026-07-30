package app

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/observability"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// recorder collects an ordered log of shutdown steps across the fakes.
type recorder struct {
	mu    sync.Mutex
	steps []string
}

func (r *recorder) add(step string) {
	r.mu.Lock()
	r.steps = append(r.steps, step)
	r.mu.Unlock()
}

func (r *recorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.steps...)
}

// fakeLeg implements ShutdownLeg and leg.RootSpanEnder, recording the order
// in which the shutdown sequence drives it.
type fakeLeg struct {
	name string
	rec  *recorder
}

func (f *fakeLeg) Hangup(context.Context) error { f.rec.add("hangup:" + f.name); return nil }
func (f *fakeLeg) EndRootSpan(reason string)    { f.rec.add("end:" + f.name + ":" + reason) }

// fakeFlusher records the flush.
type fakeFlusher struct {
	rec *recorder
	err error
}

func (f *fakeFlusher) Shutdown(context.Context) error { f.rec.add("flush"); return f.err }

type fakeHTTP struct{ rec *recorder }

func (f *fakeHTTP) Shutdown(context.Context) error { f.rec.add("http"); return nil }

type fakeTrunks struct{ rec *recorder }

func (f *fakeTrunks) Shutdown(context.Context) { f.rec.add("trunks") }

type fakeCloser struct{ rec *recorder }

func (f *fakeCloser) Close() error { f.rec.add("moq"); return nil }

// TestGracefulShutdownEndsSpansThenFlushes is criterion 3's guard.
//
// It goes RED if the flush is moved before the hangup/end loop, and RED if
// the EndRootSpan call is dropped from the loop. Both mutations silently
// destroy the trace of every leg alive at shutdown: an unended span is never
// enqueued to the batch processor, so a flush that runs first exports nothing.
func TestGracefulShutdownEndsSpansThenFlushes(t *testing.T) {
	rec := &recorder{}
	legs := []ShutdownLeg{
		&fakeLeg{name: "a", rec: rec},
		&fakeLeg{name: "b", rec: rec},
	}

	GracefulShutdown(context.Background(), ShutdownDeps{
		HTTP:   &fakeHTTP{rec: rec},
		MoQ:    &fakeCloser{rec: rec},
		Trunks: &fakeTrunks{rec: rec},
		Legs:   func() []ShutdownLeg { return legs },
		Tracer: &fakeFlusher{rec: rec},
	})

	got := rec.snapshot()

	// The serve-stopping steps are still strictly ordered, and all of them
	// precede any leg teardown.
	wantPrefix := []string{"http", "moq", "trunks"}
	if len(got) < len(wantPrefix) || !reflect.DeepEqual(got[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("shutdown prefix:\n got = %v\nwant it to start with %v", got, wantPrefix)
	}

	// Legs are torn down concurrently — one unresponsive peer must not spend
	// the budget the others need — so their order relative to EACH OTHER is
	// deliberately undefined and must not be asserted. What must hold per leg
	// is that it was hung up, its span was ended, and the span was ended after
	// its own hangup so the span covers the teardown.
	idx := func(step string) int { return slices.Index(got, step) }
	for _, name := range []string{"a", "b"} {
		h, e := idx("hangup:"+name), idx("end:"+name+":shutdown")
		if h < 0 {
			t.Errorf("leg %s was never hung up: %v", name, got)
		}
		if e < 0 {
			t.Errorf("leg %s's root span was never ended — it is then never exported: %v", name, got)
		}
		if h >= 0 && e >= 0 && e < h {
			t.Errorf("leg %s's span ended before its own hangup, so it does not cover the teardown: %v", name, got)
		}
	}

	// The two load-bearing properties, stated so a failure names the broken
	// invariant rather than a sequence mismatch.
	if got[len(got)-1] != "flush" {
		t.Errorf("flush is not last (%v) — spans ended after the flush are never exported", got)
	}
	for i, step := range got {
		if step == "flush" {
			for _, later := range got[i:] {
				if strings.HasPrefix(later, "end:") {
					t.Errorf("span ended after the flush, so it is never exported: %v", got)
				}
			}
		}
	}
}

// TestGracefulShutdownEndsEveryLegSpan pins that no live leg is skipped.
func TestGracefulShutdownEndsEveryLegSpan(t *testing.T) {
	rec := &recorder{}
	var legs []ShutdownLeg
	for _, name := range []string{"a", "b", "c"} {
		legs = append(legs, &fakeLeg{name: name, rec: rec})
	}

	GracefulShutdown(context.Background(), ShutdownDeps{
		Legs:   func() []ShutdownLeg { return legs },
		Tracer: &fakeFlusher{rec: rec},
	})

	ends := 0
	for _, step := range rec.snapshot() {
		if len(step) > 4 && step[:4] == "end:" {
			ends++
		}
	}
	if ends != 3 {
		t.Errorf("ended %d root spans, want 3 (one per live leg)", ends)
	}
}

// TestGracefulShutdownNilDepsNoPanic — every dep is optional, and shutdown
// must not panic the process on the way out.
func TestGracefulShutdownNilDepsNoPanic(t *testing.T) {
	GracefulShutdown(context.Background(), ShutdownDeps{})
}

// TestGracefulShutdownFlushErrorTolerated — a collector that is already gone
// must not stop shutdown.
func TestGracefulShutdownFlushErrorTolerated(t *testing.T) {
	rec := &recorder{}
	GracefulShutdown(context.Background(), ShutdownDeps{
		Legs:   func() []ShutdownLeg { return []ShutdownLeg{&fakeLeg{name: "a", rec: rec}} },
		Tracer: &fakeFlusher{rec: rec, err: errors.New("collector unreachable")},
	})
	if got, want := rec.snapshot(), []string{"hangup:a", "end:a:shutdown", "flush"}; !reflect.DeepEqual(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

// TestGracefulShutdownNonSpanLegSkipped — the six non-SIP leg types do not
// implement RootSpanEnder; the assertion must simply miss them.
func TestGracefulShutdownNonSpanLegSkipped(t *testing.T) {
	rec := &recorder{}
	GracefulShutdown(context.Background(), ShutdownDeps{
		Legs:   func() []ShutdownLeg { return []ShutdownLeg{&plainLeg{rec: rec}} },
		Tracer: &fakeFlusher{rec: rec},
	})
	if got, want := rec.snapshot(), []string{"hangup:plain", "flush"}; !reflect.DeepEqual(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

// plainLeg has no root span, like the non-SIP leg implementations.
type plainLeg struct{ rec *recorder }

func (p *plainLeg) Hangup(context.Context) error { p.rec.add("hangup:plain"); return nil }

// ctxFlusher snapshots the state of the context the flush is handed, which is
// what decides whether the batch span processor exports or discards its queue.
// The state is read inside Shutdown because the caller cancels the flush
// context on return; the live context object is useless afterwards.
type ctxFlusher struct {
	called    bool
	err       error
	remaining time.Duration
	unbounded bool
}

func (f *ctxFlusher) Shutdown(ctx context.Context) error {
	f.called = true
	f.err = ctx.Err()
	deadline, ok := ctx.Deadline()
	f.unbounded = !ok
	if ok {
		f.remaining = time.Until(deadline)
	}
	return nil
}

// hungTrunks models a registrar that is unreachable with no RST: its shutdown
// blocks until the caller's budget is gone, exactly as the real one does.
type hungTrunks struct{}

func (hungTrunks) Shutdown(ctx context.Context) { <-ctx.Done() }

// TestGracefulShutdownFlushSurvivesSpentBudget is the guard on the flush
// budget. An earlier step burning the caller's whole deadline must not reach
// the flush: sdktrace's Shutdown selects on ctx.Done() and returns without
// exporting, so a spent context silently discards every leg span the hangup
// loop just ended — the trace an operator most wants from a sick process.
// ctxLeg snapshots the state of the context its Hangup is handed. A real
// Hangup sends BYE on that context, so an already-dead one means no BYE is
// sent at all and the peer keeps a call the process has forgotten.
type ctxLeg struct {
	mu        sync.Mutex
	called    bool
	err       error
	unbounded bool
	remaining time.Duration
}

func (l *ctxLeg) Hangup(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.called = true
	l.err = ctx.Err()
	deadline, ok := ctx.Deadline()
	l.unbounded = !ok
	if ok {
		l.remaining = time.Until(deadline)
	}
	return nil
}

// TestGracefulShutdownHangupSurvivesSpentBudget is the guard on the hangup
// budget, and the sibling of the flush guard below.
//
// Trunks.Shutdown shares the caller's deadline and can consume all of it — an
// unreachable registrar does exactly that. If the leg loop then ran on the
// caller's context, every leg would be handed a dead context and no BYE would
// be sent, silently stranding every live call on the far end.
func TestGracefulShutdownHangupSurvivesSpentBudget(t *testing.T) {
	l := &ctxLeg{}

	// Small enough that hungTrunks consumes all of it before the legs are
	// reached, mirroring the 5s in main.go being eaten by a dead registrar.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	GracefulShutdown(ctx, ShutdownDeps{
		Trunks: hungTrunks{},
		Legs:   func() []ShutdownLeg { return []ShutdownLeg{l} },
	})

	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.called {
		t.Fatal("leg was never hung up")
	}
	if l.err != nil {
		t.Fatalf("hangup got an already-dead context (%v) — no BYE is sent and the peer keeps the call", l.err)
	}
	if l.unbounded {
		t.Fatal("hangup context has no deadline — one unresponsive peer would block shutdown forever")
	}
	if l.remaining <= 0 || l.remaining > hangupBudget {
		t.Errorf("hangup budget = %v, want (0, %v]", l.remaining, hangupBudget)
	}
}

func TestGracefulShutdownFlushSurvivesSpentBudget(t *testing.T) {
	flusher := &ctxFlusher{}
	rec := &recorder{}

	// A budget small enough that hungTrunks consumes all of it, mirroring the
	// 5s in main.go being eaten before the flush is reached.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	GracefulShutdown(ctx, ShutdownDeps{
		Trunks: hungTrunks{},
		Legs:   func() []ShutdownLeg { return []ShutdownLeg{&fakeLeg{name: "a", rec: rec}} },
		Tracer: flusher,
	})

	if !flusher.called {
		t.Fatal("flush was never called")
	}
	if flusher.err != nil {
		t.Fatalf("flush got an already-dead context (%v) — sdktrace returns on ctx.Done() and drops every queued span", flusher.err)
	}

	// The fresh deadline must still be bounded, or a hung collector would hang
	// the process past its termination grace period instead of the budget.
	if flusher.unbounded {
		t.Fatal("flush context has no deadline — a hung collector would block shutdown forever")
	}
	if flusher.remaining <= 0 || flusher.remaining > flushBudget {
		t.Errorf("flush budget = %v, want (0, %v]", flusher.remaining, flushBudget)
	}
}

// --- InstallTracing ---

// spyInstall swaps the tracing indirections and returns counters for what
// InstallTracing installed.
func spyInstall(t *testing.T, tp *sdktrace.TracerProvider, err error) (setupCalls, providerCalls, propagatorCalls *int) {
	t.Helper()
	var sc, pc, prc int

	origSetup, origTP, origProp := setupTracing, setTracerProvider, setTextMapPropagator
	setupTracing = func(context.Context, observability.Config) (*sdktrace.TracerProvider, error) {
		sc++
		return tp, err
	}
	setTracerProvider = func(trace.TracerProvider) { pc++ }
	setTextMapPropagator = func(propagation.TextMapPropagator) { prc++ }
	t.Cleanup(func() {
		setupTracing, setTracerProvider, setTextMapPropagator = origSetup, origTP, origProp
	})
	return &sc, &pc, &prc
}

// TestInstallTracingDisabledInstallsNothing is criterion 1's guard on the
// install side: a disabled config must leave both process globals untouched,
// so the OTel API's default noop tracer provider stays in place.
func TestInstallTracingDisabledInstallsNothing(t *testing.T) {
	_, providerCalls, propagatorCalls := spyInstall(t, nil, nil)

	flusher, err := InstallTracing(context.Background(), config.Config{OTELTracesEnabled: false}, "dev", nil)
	if err != nil {
		t.Fatalf("InstallTracing(disabled) error = %v", err)
	}
	if flusher != nil {
		t.Errorf("InstallTracing(disabled) returned flusher %v, want nil", flusher)
	}
	if *providerCalls != 0 {
		t.Errorf("InstallTracing(disabled) installed a tracer provider %d time(s), want 0", *providerCalls)
	}
	if *propagatorCalls != 0 {
		t.Errorf("InstallTracing(disabled) installed a propagator %d time(s), want 0", *propagatorCalls)
	}
}

// TestInstallTracingEnabledInstallsGlobals is the positive control proving
// the spies above are wired to the path the disabled test asserts is skipped.
func TestInstallTracingEnabledInstallsGlobals(t *testing.T) {
	tp := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	_, providerCalls, propagatorCalls := spyInstall(t, tp, nil)

	flusher, err := InstallTracing(context.Background(), config.Config{
		OTELTracesEnabled:  true,
		OTELTracesEndpoint: "localhost:4317",
	}, "dev", nil)
	if err != nil {
		t.Fatalf("InstallTracing(enabled) error = %v", err)
	}
	if flusher == nil {
		t.Fatal("InstallTracing(enabled) returned nil flusher, want the provider")
	}
	if *providerCalls != 1 {
		t.Errorf("installed tracer provider %d time(s), want 1", *providerCalls)
	}
	if *propagatorCalls != 1 {
		t.Errorf("installed propagator %d time(s), want 1", *propagatorCalls)
	}
}

// TestInstallTracingErrorInstallsNothing — a bad exporter config must not
// leave half a pipeline installed, and must not be fatal.
func TestInstallTracingErrorInstallsNothing(t *testing.T) {
	sentinel := errors.New("bad endpoint")
	_, providerCalls, propagatorCalls := spyInstall(t, nil, sentinel)

	flusher, err := InstallTracing(context.Background(), config.Config{OTELTracesEnabled: true}, "dev", nil)
	if !errors.Is(err, sentinel) {
		t.Errorf("error = %v, want %v", err, sentinel)
	}
	if flusher != nil {
		t.Errorf("flusher = %v, want nil", flusher)
	}
	if *providerCalls != 0 || *propagatorCalls != 0 {
		t.Errorf("installed globals (%d provider, %d propagator) despite setup error, want 0/0", *providerCalls, *propagatorCalls)
	}
}

// TestInstallTracingMapsConfig pins the config translation, including the
// instance ID landing on service.instance.id and the version attribute.
func TestInstallTracingMapsConfig(t *testing.T) {
	var got observability.Config
	orig := setupTracing
	setupTracing = func(_ context.Context, c observability.Config) (*sdktrace.TracerProvider, error) {
		got = c
		return nil, nil
	}
	t.Cleanup(func() { setupTracing = orig })

	_, err := InstallTracing(context.Background(), config.Config{
		InstanceID:           "instance-7",
		OTELTracesEnabled:    true,
		OTELTracesEndpoint:   "collector:4317",
		OTELTracesInsecure:   true,
		OTELHeaders:          "authorization=Bearer t",
		OTELServiceName:      "vb-edge",
		OTELServiceNamespace: "telephony",
		OTELPropagators:      "tracecontext",
		OTELSamplerRatio:     0.25,
	}, "v1.2.3", nil)
	if err != nil {
		t.Fatalf("InstallTracing: %v", err)
	}

	want := observability.Config{
		Enabled:          true,
		Endpoint:         "collector:4317",
		Insecure:         true,
		Headers:          map[string]string{"authorization": "Bearer t"},
		ServiceName:      "vb-edge",
		ServiceVersion:   "v1.2.3",
		ServiceNamespace: "telephony",
		InstanceID:       "instance-7",
		Propagators:      "tracecontext",
		SamplerRatio:     0.25,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("observability config:\n got = %+v\nwant = %+v", got, want)
	}
}
