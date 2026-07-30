package api

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// schemaProperties resolves components.schemas.<name>.properties in a spec
// document, failing the test if any hop is missing.
func schemaProperties(t *testing.T, file, schema string) *yaml.Node {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), file))
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	if len(doc.Content) == 0 {
		t.Fatalf("%s is empty", file)
	}
	n := doc.Content[0]
	for _, key := range []string{"components", "schemas", schema, "properties"} {
		n = mappingValue(n, key)
		if n == nil {
			t.Fatalf("%s: no %s under components.schemas.%s", file, key, schema)
		}
	}
	return n
}

// TestOpenAPISpec_CarriesDrainFields asserts the checked-in openapi.yaml was
// regenerated after the DeleteLegRequest change. There is no regenerate-and-diff
// gate in the repo, so without this a forgotten `go generate` ships silently.
//
// The assertions are scoped to the DeleteLegRequest schema node rather than
// grepping the file: both field names also appear in the deleteLeg operation
// description, so a substring search over the whole document would pass even
// with the properties missing.
func TestOpenAPISpec_CarriesDrainFields(t *testing.T) {
	props := schemaProperties(t, "openapi.yaml", "DeleteLegRequest")

	drain := mappingValue(props, "drain_playback")
	if drain == nil {
		t.Fatal("openapi.yaml: DeleteLegRequest has no drain_playback; run go generate ./internal/api/")
	}
	if v := mappingValue(drain, "type"); v == nil || v.Value != "boolean" {
		t.Errorf("drain_playback type = %v, want boolean", v)
	}

	budget := mappingValue(props, "drain_timeout_ms")
	if budget == nil {
		t.Fatal("openapi.yaml: DeleteLegRequest has no drain_timeout_ms; run go generate ./internal/api/")
	}
	// The enrichment constraints are the only evidence the FieldEnrichment was
	// wired through; the bare property would be emitted from the struct alone.
	for _, tc := range []struct{ key, want string }{
		{"type", "integer"},
		{"default", "5000"},
		{"minimum", "0"},
		{"maximum", "30000"},
	} {
		got := mappingValue(budget, tc.key)
		if got == nil || got.Value != tc.want {
			t.Errorf("drain_timeout_ms %s = %v, want %s", tc.key, got, tc.want)
		}
	}
}

// TestAsyncAPISpec_CarriesDrainFields is the VSI mirror. Nothing else in the
// tree asserts asyncapi.yaml at all, so this is the only thing standing between
// a delete_leg payload change and a stale published spec.
func TestAsyncAPISpec_CarriesDrainFields(t *testing.T) {
	props := schemaProperties(t, "asyncapi.yaml", "deleteLegPayload")

	// cmd/asyncapi-gen does not consume SchemaEnrichments, so these land as
	// bare typed properties beside the equally bare `reason`. Expected output,
	// not drift — assert the types only.
	for _, tc := range []struct{ name, want string }{
		{"drain_playback", "boolean"},
		{"drain_timeout_ms", "integer"},
	} {
		p := mappingValue(props, tc.name)
		if p == nil {
			t.Errorf("asyncapi.yaml: deleteLegPayload has no %s; run go generate ./internal/api/", tc.name)
			continue
		}
		if v := mappingValue(p, "type"); v == nil || v.Value != tc.want {
			t.Errorf("%s type = %v, want %s", tc.name, v, tc.want)
		}
	}
}
