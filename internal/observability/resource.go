package observability

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
)

// CreateResource builds the resource describing this VoiceBlender instance.
// service.instance.id defaults from the process instance ID so spans join the
// same instance row the event bus and metrics already stamp.
func CreateResource(ctx context.Context, cfg Config) (*resource.Resource, error) {
	attrs := collectAttributes(cfg)

	res, err := resource.New(ctx,
		resource.WithAttributes(attrs...),
		resource.WithProcess(),
		resource.WithHost(),
		resource.WithTelemetrySDK(),
		resource.WithFromEnv(),
	)
	// Guard on the resource, not the error. resource.New returns a usable
	// resource alongside a partial-detection error — an unresolvable process
	// owner (distroless/scratch, where user.Current() fails) or a malformed
	// OTEL_RESOURCE_ATTRIBUTES entry both land here with every other
	// attribute intact. A cosmetic detector failure must not disable the
	// pipeline the operator explicitly enabled, matching Ratio()'s clamp and
	// ParseHeaders' skip-malformed.
	if res == nil {
		return nil, fmt.Errorf("create resource: %w", err)
	}
	return res, nil
}

func collectAttributes(cfg Config) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 4)
	if cfg.ServiceName != "" {
		attrs = append(attrs, semconv.ServiceNameKey.String(cfg.ServiceName))
	}
	if cfg.ServiceVersion != "" {
		attrs = append(attrs, semconv.ServiceVersionKey.String(cfg.ServiceVersion))
	}
	if cfg.ServiceNamespace != "" {
		attrs = append(attrs, semconv.ServiceNamespaceKey.String(cfg.ServiceNamespace))
	}
	if cfg.InstanceID != "" {
		attrs = append(attrs, semconv.ServiceInstanceIDKey.String(cfg.InstanceID))
	}
	// host.name is deliberately absent: resource.WithHost() detects it with
	// the identical key, value and source, at the SDK's own semconv version.
	return attrs
}
