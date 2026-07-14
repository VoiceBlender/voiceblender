package observability

import (
	"context"
	"fmt"
	"os"

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
	if err != nil {
		return nil, fmt.Errorf("create resource: %w", err)
	}
	return res, nil
}

func collectAttributes(cfg Config) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 5)
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
	if hostname, err := os.Hostname(); err == nil && hostname != "" {
		attrs = append(attrs, semconv.HostNameKey.String(hostname))
	}
	return attrs
}
