package observability

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// exporterSpy records what Setup asked of newTraceExporter: how many times,
// with which options, and it hands back an in-memory exporter so a test can
// see the spans the provider actually samples.
type exporterSpy struct {
	calls int
	opts  []otlptracegrpc.Option
	exp   *tracetest.InMemoryExporter
}

// spyExporter swaps newTraceExporter for the duration of a test.
func spyExporter(t *testing.T) *exporterSpy {
	t.Helper()
	s := &exporterSpy{exp: tracetest.NewInMemoryExporter()}
	orig := newTraceExporter
	newTraceExporter = func(_ context.Context, opts ...otlptracegrpc.Option) (sdktrace.SpanExporter, error) {
		s.calls++
		s.opts = opts
		return s.exp, nil
	}
	t.Cleanup(func() { newTraceExporter = orig })
	return s
}

// TestSetupDisabledDialsNothing is criterion 1's guard: on the default
// disabled path Setup must return a nil provider AND never construct an
// exporter. Asserting the config flag would prove nothing; the spy call
// count is what proves no exporter is dialed.
func TestSetupDisabledDialsNothing(t *testing.T) {
	spy := spyExporter(t)

	// An endpoint is configured on purpose: disabled must win over it.
	tp, err := Setup(context.Background(), Config{
		Enabled:  false,
		Endpoint: "localhost:4317",
	})
	if err != nil {
		t.Fatalf("Setup(disabled) returned error: %v", err)
	}
	if tp != nil {
		t.Errorf("Setup(disabled) returned provider %v, want nil", tp)
	}
	if spy.calls != 0 {
		t.Errorf("Setup(disabled) constructed %d exporter(s), want 0 — the disabled path must dial nothing", spy.calls)
	}
}

// TestSetupEnabledConstructsExporter is the positive control for the spy: it
// proves the spy is actually wired to the path TestSetupDisabledDialsNothing
// asserts is not taken.
func TestSetupEnabledConstructsExporter(t *testing.T) {
	spy := spyExporter(t)

	tp, err := Setup(context.Background(), Config{
		Enabled:     true,
		Endpoint:    "localhost:4317",
		Insecure:    true,
		ServiceName: "voiceblender",
	})
	if err != nil {
		t.Fatalf("Setup(enabled) returned error: %v", err)
	}
	if tp == nil {
		t.Fatal("Setup(enabled) returned nil provider, want a provider")
	}
	if spy.calls != 1 {
		t.Errorf("Setup(enabled) constructed %d exporter(s), want 1", spy.calls)
	}
	_ = tp.Shutdown(context.Background())
}

func TestSetupEnabledEmptyEndpointErrors(t *testing.T) {
	spy := spyExporter(t)

	tp, err := Setup(context.Background(), Config{Enabled: true, Endpoint: ""})
	if !errors.Is(err, ErrEndpointRequired) {
		t.Errorf("Setup(enabled, no endpoint) error = %v, want ErrEndpointRequired", err)
	}
	if tp != nil {
		t.Errorf("Setup(enabled, no endpoint) returned provider %v, want nil", tp)
	}
	if spy.calls != 0 {
		t.Errorf("Setup(enabled, no endpoint) constructed %d exporter(s), want 0", spy.calls)
	}
}

func TestSetupExporterErrorPropagates(t *testing.T) {
	sentinel := errors.New("dial refused")
	orig := newTraceExporter
	newTraceExporter = func(context.Context, ...otlptracegrpc.Option) (sdktrace.SpanExporter, error) {
		return nil, sentinel
	}
	t.Cleanup(func() { newTraceExporter = orig })

	tp, err := Setup(context.Background(), Config{Enabled: true, Endpoint: "localhost:4317"})
	if !errors.Is(err, sentinel) {
		t.Errorf("Setup error = %v, want it to wrap %v", err, sentinel)
	}
	if tp != nil {
		t.Errorf("Setup returned provider %v on exporter error, want nil", tp)
	}
}

// TestSetupHonoursSamplerRatio proves the configured ratio actually reaches
// the provider's sampler. Ratio() is exhaustively table-tested as a pure
// function, but nothing proved its result was ever used: swapping the sampler
// for AlwaysSample() left every test green while OTEL_TRACES_SAMPLER_ARG
// became a documented no-op knob.
func TestSetupHonoursSamplerRatio(t *testing.T) {
	const spans = 20

	record := func(t *testing.T, ratio float64) int {
		t.Helper()
		spy := spyExporter(t)
		tp, err := Setup(context.Background(), Config{
			Enabled:      true,
			Endpoint:     "localhost:4317",
			SamplerRatio: ratio,
		})
		if err != nil {
			t.Fatalf("Setup: %v", err)
		}
		tracer := tp.Tracer("t")
		for i := 0; i < spans; i++ {
			_, span := tracer.Start(context.Background(), "s")
			span.End()
		}
		if err := tp.ForceFlush(context.Background()); err != nil {
			t.Fatalf("ForceFlush: %v", err)
		}
		t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
		return len(spy.exp.GetSpans())
	}

	if got := record(t, 0); got != 0 {
		t.Errorf("exported %d spans at SamplerRatio 0, want 0", got)
	}
	if got := record(t, 1); got != spans {
		t.Errorf("exported %d spans at SamplerRatio 1, want %d", got, spans)
	}
}

// TestSetupPassesExporterOptions is a STRUCTURAL count, not a semantic proof.
// otlptracegrpc.Option values are opaque unexported config mutators that
// cannot be inspected without reflection hacks, so this catches deletion of
// WithHeaders/WithInsecure and nothing more — it cannot prove the header map
// or the endpoint arrive intact at the collector. Stated plainly because a
// guard that overclaims is worse than no guard.
func TestSetupPassesExporterOptions(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want int
	}{
		{
			name: "endpoint only",
			cfg:  Config{Enabled: true, Endpoint: "localhost:4317"},
			want: 1,
		},
		{
			name: "endpoint, insecure and headers",
			cfg: Config{
				Enabled:  true,
				Endpoint: "localhost:4317",
				Insecure: true,
				Headers:  map[string]string{"authorization": "Bearer t"},
			},
			want: 3,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spy := spyExporter(t)
			tp, err := Setup(context.Background(), tc.cfg)
			if err != nil {
				t.Fatalf("Setup: %v", err)
			}
			t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
			if got := len(spy.opts); got != tc.want {
				t.Errorf("Setup passed %d exporter options, want %d", got, tc.want)
			}
		})
	}
}

func TestCreateResource(t *testing.T) {
	res, err := CreateResource(context.Background(), Config{
		ServiceName:      "voiceblender",
		ServiceVersion:   "1.2.3",
		ServiceNamespace: "telephony",
		InstanceID:       "instance-abc",
	})
	if err != nil {
		t.Fatalf("CreateResource: %v", err)
	}

	want := map[string]string{
		"service.name":        "voiceblender",
		"service.version":     "1.2.3",
		"service.namespace":   "telephony",
		"service.instance.id": "instance-abc",
	}
	got := make(map[string]string)
	for _, kv := range res.Attributes() {
		got[string(kv.Key)] = kv.Value.Emit()
	}
	for key, wantVal := range want {
		if got[key] != wantVal {
			t.Errorf("resource attribute %q = %q, want %q", key, got[key], wantVal)
		}
	}
}

func TestPropagator(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		fields []string
	}{
		{"empty yields default composite", "", []string{"traceparent", "tracestate", "baggage"}},
		{"named list", "tracecontext,baggage", []string{"traceparent", "tracestate", "baggage"}},
		{"tracecontext only", "tracecontext", []string{"traceparent", "tracestate"}},
		{"baggage only", "baggage", []string{"baggage"}},
		{"unknown ignored, falls back to default", "b3,jaeger", []string{"traceparent", "tracestate", "baggage"}},
		{"unknown dropped from known list", "tracecontext,nonsense", []string{"traceparent", "tracestate"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Fields() order is not stable (the composite propagator dedups
			// through a map), so compare as a set.
			got := append([]string(nil), Propagator(Config{Propagators: tc.in}).Fields()...)
			want := append([]string(nil), tc.fields...)
			sort.Strings(got)
			sort.Strings(want)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("Fields() = %v, want %v", got, want)
			}
		})
	}
}
