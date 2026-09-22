package audiofilter

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	specs, err := Parse("bandpass:low_hz=300:high_hz=3400,gain:volume=-2")
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 2 || specs[0].Type != "bandpass" || specs[1].Type != "gain" {
		t.Fatalf("got %+v", specs)
	}
	if specs[0].Params["low_hz"] != 300 || specs[0].Params["high_hz"] != 3400 {
		t.Errorf("bandpass params: %+v", specs[0].Params)
	}
	if specs[1].Params["volume"] != -2 {
		t.Errorf("gain params: %+v", specs[1].Params)
	}
	t.Logf("parsed %+v", specs)

	if s, err := Parse("  "); err != nil || s != nil {
		t.Errorf("empty list should parse to nil, got %+v %v", s, err)
	}
	t.Logf("registered filters: %v", Names())
}

func TestParseRejects(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"nosuchfilter", "unknown filter"},
		{"bandpass,nosuchfilter", "unknown filter"},
		{"bandpass:oops", "not key=value"},
		{"bandpass:low_hz=abc", "parameter"},
		{"gain,gain,gain,gain,gain", "maximum"},
	} {
		_, err := Parse(c.in)
		if err == nil {
			t.Errorf("%q: expected an error", c.in)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: error %q does not mention %q", c.in, err, c.want)
			continue
		}
		t.Logf("%-28s -> %v", c.in, err)
	}
}

// TestValidateUnique covers the duplicate guard that stops a chain quietly
// acquiring two of a costly per-stream resource.
func TestValidateUnique(t *testing.T) {
	Register("uniquetest", Descriptor{Unique: true,
		New: func(int, Params) (Stage, error) { return &gainStage{gain: 1}, nil }})
	defer Unregister("uniquetest")

	if err := Validate([]Spec{{Type: "uniquetest"}}); err != nil {
		t.Fatalf("single use should be valid: %v", err)
	}
	err := Validate([]Spec{{Type: "uniquetest"}, {Type: "uniquetest"}})
	if err == nil || !strings.Contains(err.Error(), "only once") {
		t.Fatalf("duplicate should be rejected, got %v", err)
	}
	t.Logf("duplicate rejected: %v", err)

	if err := Validate([]Spec{{Type: "gain"}, {Type: "gain"}}); err != nil {
		t.Errorf("non-unique filters may repeat: %v", err)
	}
}

func TestUnknownFilterBuildFails(t *testing.T) {
	_, err := Build(&chunkSource{}, 16000, 16000, []Spec{{Type: "nope"}})
	if err == nil || !strings.Contains(err.Error(), "unknown filter") {
		t.Fatalf("expected unknown-filter error, got %v", err)
	}
	if !strings.Contains(err.Error(), "bandpass") {
		t.Errorf("error should list known filters, got %q", err)
	}
	t.Logf("%v", err)
}

func TestRegisterRejectsDuplicate(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("registering a duplicate should panic")
		} else {
			t.Logf("panicked as expected: %v", r)
		}
	}()
	Register("gain", Descriptor{New: func(int, Params) (Stage, error) { return nil, nil }})
}

// TestResolveDropsUnavailable covers degraded mode: a filter whose backing
// resource failed to initialise must not fail call setup, and the caller must
// be able to report what actually runs.
func TestResolveDropsUnavailable(t *testing.T) {
	up := true
	Register("flaky", Descriptor{
		Available: func() bool { return up },
		New:       func(int, Params) (Stage, error) { return &gainStage{gain: 1}, nil },
	})
	defer Unregister("flaky")

	in := []Spec{{Type: "bandpass"}, {Type: "flaky"}, {Type: "gain"}}
	if got := Resolve(in); len(got) != 3 {
		t.Fatalf("available filter should be kept, got %+v", got)
	}

	up = false
	got := Resolve(in)
	if len(got) != 2 || got[0].Type != "bandpass" || got[1].Type != "gain" {
		t.Fatalf("unavailable filter should be dropped, got %+v", got)
	}
	t.Logf("unavailable filter dropped: %+v -> %+v", in, got)

	// Dropping must leave a buildable chain, not an error.
	if _, err := Build(&chunkSource{}, 16000, 16000, got); err != nil {
		t.Fatalf("resolved chain must build: %v", err)
	}
	// An unknown filter is still an error, not something Resolve hides.
	if _, err := Build(&chunkSource{}, 16000, 16000, Resolve([]Spec{{Type: "nope"}})); err == nil {
		t.Error("unknown filter must still fail")
	}
}
