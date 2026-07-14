package app

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

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

	want := []string{
		"http", "moq", "trunks",
		"hangup:a", "end:a:shutdown",
		"hangup:b", "end:b:shutdown",
		"flush",
	}
	got := rec.snapshot()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("shutdown order:\n got = %v\nwant = %v", got, want)
	}

	// State the two load-bearing properties independently of the exact
	// sequence, so the failure message names the broken invariant.
	if got[len(got)-1] != "flush" {
		t.Errorf("flush is not last (%v) — spans ended after the flush are never exported", got)
	}
	for i, step := range got {
		if step == "flush" {
			for _, later := range got[i:] {
				if len(later) > 4 && later[:4] == "end:" {
					t.Errorf("span ended after the flush: %v", got)
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
