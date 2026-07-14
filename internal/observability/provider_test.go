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

// spyExporter swaps newTraceExporter for the duration of a test and counts
// how many times Setup asked for an exporter.
func spyExporter(t *testing.T) *int {
	t.Helper()
	calls := 0
	orig := newTraceExporter
	newTraceExporter = func(context.Context, ...otlptracegrpc.Option) (sdktrace.SpanExporter, error) {
		calls++
		return tracetest.NewInMemoryExporter(), nil
	}
	t.Cleanup(func() { newTraceExporter = orig })
	return &calls
}

// TestSetupDisabledDialsNothing is criterion 1's guard: on the default
// disabled path Setup must return a nil provider AND never construct an
// exporter. Asserting the config flag would prove nothing; the spy call
// count is what proves no exporter is dialed.
func TestSetupDisabledDialsNothing(t *testing.T) {
	calls := spyExporter(t)

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
	if *calls != 0 {
		t.Errorf("Setup(disabled) constructed %d exporter(s), want 0 — the disabled path must dial nothing", *calls)
	}
}

// TestSetupEnabledConstructsExporter is the positive control for the spy: it
// proves the spy is actually wired to the path TestSetupDisabledDialsNothing
// asserts is not taken.
func TestSetupEnabledConstructsExporter(t *testing.T) {
	calls := spyExporter(t)

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
	if *calls != 1 {
		t.Errorf("Setup(enabled) constructed %d exporter(s), want 1", *calls)
	}
	_ = tp.Shutdown(context.Background())
}

func TestSetupEnabledEmptyEndpointErrors(t *testing.T) {
	calls := spyExporter(t)

	tp, err := Setup(context.Background(), Config{Enabled: true, Endpoint: ""})
	if !errors.Is(err, ErrEndpointRequired) {
		t.Errorf("Setup(enabled, no endpoint) error = %v, want ErrEndpointRequired", err)
	}
	if tp != nil {
		t.Errorf("Setup(enabled, no endpoint) returned provider %v, want nil", tp)
	}
	if *calls != 0 {
		t.Errorf("Setup(enabled, no endpoint) constructed %d exporter(s), want 0", *calls)
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
