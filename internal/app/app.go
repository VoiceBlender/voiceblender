// Package app holds the process wiring that used to live inline in
// func main: installing the trace pipeline at startup and running the
// graceful shutdown sequence.
//
// It exists as its own package because package main cannot be imported by a
// test. The startup and shutdown ordering here carries real guarantees — a
// span must be ended before the exporter is flushed, or it is never exported
// at all — and those guarantees are only testable from a package that
// `go test ./internal/...` can reach.
package app

import (
	"context"
	"io"
	"log/slog"

	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/VoiceBlender/voiceblender/internal/observability"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Flusher flushes buffered telemetry and stops its exporter.
// *sdktrace.TracerProvider satisfies it.
type Flusher interface {
	Shutdown(ctx context.Context) error
}

// HTTPServer is the part of *http.Server the shutdown sequence uses.
type HTTPServer interface {
	Shutdown(ctx context.Context) error
}

// TrunkRegistry is the part of *sip.TrunkManager the shutdown sequence uses.
type TrunkRegistry interface {
	Shutdown(ctx context.Context)
}

// ShutdownLeg is the part of leg.Leg the shutdown sequence uses. Narrow on
// purpose: leg.Leg carries forty-odd methods that shutdown does not touch.
type ShutdownLeg interface {
	Hangup(ctx context.Context) error
}

// Indirections so tests can observe what startup installs without mutating
// real process globals.
var (
	setupTracing         = observability.Setup
	setTracerProvider    = func(tp trace.TracerProvider) { otel.SetTracerProvider(tp) }
	setTextMapPropagator = func(p propagation.TextMapPropagator) { otel.SetTextMapPropagator(p) }
)

// InstallTracing builds the trace pipeline from cfg and, when it is enabled,
// installs it as the process-global tracer provider and propagator.
//
// When tracing is disabled it installs nothing and returns a nil Flusher —
// the OTel API's default noop tracer provider stays in place, so callers that
// ask for a tracer still get a working (non-nil) noop. The caller keeps the
// returned Flusher for shutdown.
//
// Returning an error is not fatal to the process: the caller logs it and runs
// on without traces.
func InstallTracing(ctx context.Context, cfg config.Config, version string, log *slog.Logger) (Flusher, error) {
	obsCfg := observability.Config{
		Enabled:          cfg.OTELTracesEnabled,
		Endpoint:         cfg.OTELTracesEndpoint,
		Insecure:         cfg.OTELTracesInsecure,
		Headers:          observability.ParseHeaders(cfg.OTELHeaders),
		ServiceName:      cfg.OTELServiceName,
		ServiceVersion:   version,
		ServiceNamespace: cfg.OTELServiceNamespace,
		InstanceID:       cfg.InstanceID,
		Propagators:      cfg.OTELPropagators,
		SamplerRatio:     cfg.OTELSamplerRatio,
	}

	tp, err := setupTracing(ctx, obsCfg)
	if err != nil {
		return nil, err
	}
	if tp == nil {
		// Disabled. Install no globals.
		return nil, nil
	}

	setTracerProvider(tp)
	setTextMapPropagator(observability.Propagator(obsCfg))
	if log != nil {
		log.Info("otel traces enabled",
			"endpoint", obsCfg.Endpoint,
			"insecure", obsCfg.Insecure,
			"sampler_ratio", obsCfg.Ratio(),
		)
	}
	return tp, nil
}

// Compile-time proof that the SDK provider satisfies Flusher.
var _ Flusher = (*sdktrace.TracerProvider)(nil)

// ShutdownDeps are the process components the shutdown sequence touches.
// Every field is optional; a nil field is skipped.
type ShutdownDeps struct {
	HTTP   HTTPServer
	MoQ    io.Closer
	Trunks TrunkRegistry

	// Legs lists the legs still alive at shutdown.
	Legs func() []ShutdownLeg

	// Tracer flushes buffered spans. Nil when tracing is disabled.
	Tracer Flusher

	Log *slog.Logger
}

// GracefulShutdown stops serving, hangs up every live leg, and only then
// flushes buffered spans.
//
// The flush must come last. Ending a span is what enqueues it to the batch
// span processor — the processor's OnStart is a noop, so a span that has not
// been ended is not in the exporter's queue and a flush would not export it.
// Flushing before the hangup loop would therefore silently drop the root span
// of every leg that was still up when the process was asked to stop, which is
// exactly the trace an operator wants after a restart.
func GracefulShutdown(ctx context.Context, deps ShutdownDeps) {
	if deps.HTTP != nil {
		_ = deps.HTTP.Shutdown(ctx)
	}
	if deps.MoQ != nil {
		_ = deps.MoQ.Close()
	}
	if deps.Trunks != nil {
		deps.Trunks.Shutdown(ctx)
	}

	if deps.Legs != nil {
		for _, l := range deps.Legs() {
			_ = l.Hangup(ctx)
			// Hangup does not publish leg.disconnected, so nothing else on
			// this path would ever end the leg's root span.
			if e, ok := l.(leg.RootSpanEnder); ok {
				e.EndRootSpan("shutdown")
			}
		}
	}

	if deps.Tracer != nil {
		if err := deps.Tracer.Shutdown(ctx); err != nil && deps.Log != nil {
			deps.Log.Error("flush traces on shutdown", "error", err)
		}
	}
}
