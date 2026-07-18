package observability

import (
	"context"
	"os"
	"testing"
)

// resourceAttrs flattens a resource's attribute set for lookup.
func resourceAttrs(t *testing.T, cfg Config) map[string]string {
	t.Helper()
	res, err := CreateResource(context.Background(), cfg)
	if err != nil {
		t.Fatalf("CreateResource: %v", err)
	}
	if res == nil {
		t.Fatal("CreateResource returned a nil resource")
	}
	out := make(map[string]string)
	for _, kv := range res.Attributes() {
		out[string(kv.Key)] = kv.Value.Emit()
	}
	return out
}

// TestCreateResourceSurvivesMalformedEnvAttrs pins that a cosmetic detection
// failure does not disable tracing. resource.New returns a fully usable
// resource ALONGSIDE a partial-detection error: a malformed
// OTEL_RESOURCE_ATTRIBUTES entry (here, one with no "=") is enough, and so is
// an unresolvable process owner in a distroless/scratch container. Guarding
// on the error rather than the resource turned that into a total tracing
// outage — the pipeline the operator explicitly enabled, silently off.
func TestCreateResourceSurvivesMalformedEnvAttrs(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "deployment.environment")

	res, err := CreateResource(context.Background(), Config{ServiceName: "vb"})
	if err != nil {
		t.Fatalf("CreateResource = %v; want a usable resource — a partial detection error must not disable tracing", err)
	}
	if res == nil {
		t.Fatal("CreateResource returned nil resource despite a recoverable partial detection")
	}

	var found string
	for _, kv := range res.Attributes() {
		if string(kv.Key) == "service.name" {
			found = kv.Value.Emit()
		}
	}
	if found != "vb" {
		t.Errorf("service.name = %q, want vb — the resource must survive the malformed env entry intact", found)
	}
}

// TestCreateResourceDetectsHostName proves resource.WithHost() is the sole
// source of host.name. collectAttributes used to recompute it from
// os.Hostname() with the identical key, value and source; this test is GREEN
// both with and without those lines, which is exactly what shows they were
// dead weight. It goes RED only if WithHost() itself is dropped.
func TestCreateResourceDetectsHostName(t *testing.T) {
	want, err := os.Hostname()
	if err != nil || want == "" {
		t.Skip("os.Hostname unavailable on this box")
	}

	attrs := resourceAttrs(t, Config{ServiceName: "vb"})
	if got := attrs["host.name"]; got != want {
		t.Errorf("host.name = %q, want %q — resource.WithHost() must supply it", got, want)
	}
}
