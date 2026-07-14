package observability

import (
	"context"
	"errors"
	"fmt"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// ErrEndpointRequired is returned by Setup when tracing is enabled but no
// collector endpoint is configured.
var ErrEndpointRequired = errors.New("observability: OTEL_EXPORTER_OTLP_TRACES_ENDPOINT is required when traces are enabled")

// newTraceExporter constructs the OTLP/gRPC span exporter. It is a package
// var so tests can substitute a spy and assert the disabled path constructs
// no exporter at all.
var newTraceExporter = func(ctx context.Context, opts ...otlptracegrpc.Option) (sdktrace.SpanExporter, error) {
	return otlptracegrpc.New(ctx, opts...)
}

// Setup builds a tracer provider from cfg. When tracing is disabled it
// returns (nil, nil) and constructs no exporter — "disabled" is not an error
// the caller has to sniff. The provider is returned rather than installed;
// the caller decides whether to make it global.
func Setup(ctx context.Context, cfg Config) (*sdktrace.TracerProvider, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	if cfg.Endpoint == "" {
		return nil, ErrEndpointRequired
	}

	res, err := CreateResource(ctx, cfg)
	if err != nil {
		return nil, err
	}

	opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(cfg.Endpoint)}
	if cfg.Insecure {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}
	if len(cfg.Headers) > 0 {
		opts = append(opts, otlptracegrpc.WithHeaders(cfg.Headers))
	}

	exporter, err := newTraceExporter(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("create trace exporter: %w", err)
	}

	return sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithSpanProcessor(sdktrace.NewBatchSpanProcessor(exporter)),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.Ratio()))),
	), nil
}

// Propagator builds the text-map propagator for cfg. Unknown names are
// ignored; an empty or fully-unrecognized list yields the default composite
// of W3C trace-context plus baggage.
func Propagator(cfg Config) propagation.TextMapPropagator {
	var props []propagation.TextMapPropagator
	for _, name := range cfg.PropagatorsList() {
		switch name {
		case "tracecontext":
			props = append(props, propagation.TraceContext{})
		case "baggage":
			props = append(props, propagation.Baggage{})
		}
	}
	if len(props) == 0 {
		props = []propagation.TextMapPropagator{propagation.TraceContext{}, propagation.Baggage{}}
	}
	return propagation.NewCompositeTextMapPropagator(props...)
}
