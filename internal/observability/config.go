// Package observability builds a traces-only OpenTelemetry pipeline for
// VoiceBlender. It exports spans over OTLP/gRPC and is disabled by default.
//
// The package deliberately owns no process-global state: Setup returns a
// tracer provider and leaves installation to the caller, so tests can drive
// it hermetically. Metrics and logs pipelines are out of scope; VoiceBlender
// logs to stdout via slog and exposes Prometheus metrics separately.
package observability

import (
	"strings"
)

// Config describes the trace pipeline. It is built by the caller from the
// process configuration; this package reads no environment itself.
type Config struct {
	// Enabled gates the whole pipeline. When false, Setup constructs no
	// exporter and returns a nil provider.
	Enabled bool

	// Endpoint is the OTLP/gRPC collector address (host:port). Required
	// when Enabled.
	Endpoint string

	// Insecure disables transport security on the exporter connection.
	Insecure bool

	// Headers are sent with every export request (e.g. an auth token).
	Headers map[string]string

	// Resource attributes.
	ServiceName      string
	ServiceVersion   string
	ServiceNamespace string
	InstanceID       string

	// Propagators is a comma-separated list; empty means the default
	// composite of tracecontext + baggage.
	Propagators string

	// SamplerRatio is the head-sampling probability, clamped to [0,1].
	SamplerRatio float64
}

// PropagatorsList splits the configured propagator names, lowercased and
// trimmed, dropping empties. An empty Propagators yields a nil slice, which
// ConfigurePropagators reads as "use the default composite".
func (c Config) PropagatorsList() []string {
	var out []string
	for _, name := range strings.Split(c.Propagators, ",") {
		if name = strings.ToLower(strings.TrimSpace(name)); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// Ratio returns SamplerRatio clamped to [0,1]. Out-of-range values are
// clamped rather than rejected: a bad sampler ratio must never keep the
// media server from starting.
func (c Config) Ratio() float64 {
	switch {
	case c.SamplerRatio < 0:
		return 0
	case c.SamplerRatio > 1:
		return 1
	default:
		return c.SamplerRatio
	}
}

// ParseHeaders parses an OTLP headers value: a comma-separated list of k=v
// pairs (the OTEL_EXPORTER_OTLP_HEADERS format). Malformed entries are
// skipped rather than failing the parse — a typo in a header must not stop
// the process from starting. Returns nil when nothing valid is present.
func ParseHeaders(raw string) map[string]string {
	var out map[string]string
	for _, pair := range strings.Split(raw, ",") {
		key, value, ok := strings.Cut(pair, "=")
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if !ok || key == "" || value == "" {
			continue
		}
		if out == nil {
			out = make(map[string]string)
		}
		out[key] = value
	}
	return out
}
