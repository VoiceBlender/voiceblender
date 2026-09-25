package api

import (
	"strings"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/audiofilter"
)

// TestToFilterSpecs covers the three-way meaning of the request field, which
// the room relies on to decide whether the server default applies.
func TestToFilterSpecs(t *testing.T) {
	s := newTestServer(t)
	got, err := s.toFilterSpecs(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("an omitted field must stay nil so the server default applies, got %+v", got)
	}

	got, err = s.toFilterSpecs([]FilterSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("an explicit empty array must be a non-nil empty chain, got %+v", got)
	}
	t.Log("omitted -> nil (use default); [] -> empty non-nil (explicit opt-out)")

	got, err = s.toFilterSpecs([]FilterSpec{{Type: "bandpass", Params: map[string]float64{"low_hz": 400}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Type != "bandpass" || got[0].Params["low_hz"] != 400 {
		t.Fatalf("params did not survive conversion: %+v", got)
	}
	t.Logf("converted: %+v", got)
}

func TestToFilterSpecsRejects(t *testing.T) {
	srv := newTestServer(t)
	for _, c := range []struct {
		name string
		in   []FilterSpec
		want string
	}{
		{"unknown filter", []FilterSpec{{Type: "nope"}}, "unknown filter"},
		{"chain too long", []FilterSpec{{Type: "gain"}, {Type: "gain"}, {Type: "gain"}, {Type: "gain"}, {Type: "gain"}}, "maximum"},
	} {
		_, err := srv.toFilterSpecs(c.in)
		if err == nil {
			t.Errorf("%s: expected rejection", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %q should mention %q", c.name, err, c.want)
			continue
		}
		t.Logf("%-16s -> %v", c.name, err)
	}
}

// TestEffectiveFiltersReported checks the leg view reports what will actually
// run, so an operator can tell a configured chain from a dropped one.
func TestEffectiveFiltersReported(t *testing.T) {
	s := newTestServer(t)
	def := []audiofilter.Spec{{Type: "gain", Params: audiofilter.Params{"volume": 2}}}
	s.RoomMgr.SetDefaultFilters(def)

	l := &apiMockLeg{id: "leg-a"}
	if got := s.effectiveFilters(l); len(got) != 1 || got[0].Type != "gain" {
		t.Fatalf("a leg that chose nothing should report the server default, got %+v", got)
	}

	l.SetFilters([]audiofilter.Spec{})
	if got := s.effectiveFilters(l); len(got) != 0 {
		t.Fatalf("an opt-out leg should report no filters, got %+v", got)
	}

	l.SetFilters([]audiofilter.Spec{{Type: "bandpass"}})
	view := s.toLegView(l)
	if len(view.Filters) != 1 || view.Filters[0].Type != "bandpass" {
		t.Fatalf("leg view should report the leg's own chain, got %+v", view.Filters)
	}
	t.Logf("leg view reports: %+v", view.Filters)

	// A view with no processing omits the field rather than sending [].
	l.SetFilters([]audiofilter.Spec{})
	if v := s.toLegView(l); v.Filters != nil {
		t.Errorf("no processing should omit the field, got %+v", v.Filters)
	}
}

// TestDefaultFilterParsing pins the startup behaviour: a bad AUDIO_FILTERS
// value must not be silently treated as a valid empty chain.
func TestDefaultFilterParsing(t *testing.T) {
	if _, err := audiofilter.Parse("denoise,nosuchfilter"); err == nil {
		t.Error("a bad AUDIO_FILTERS value must report an error for the caller to log")
	}
	specs, err := audiofilter.Parse("bandpass:low_hz=300,gain:volume=-1")
	if err != nil {
		t.Fatal(err)
	}
	if audiofilter.Format(specs) != "bandpass:low_hz=300,gain:volume=-1" {
		t.Errorf("Format should round-trip Parse, got %q", audiofilter.Format(specs))
	}
	t.Logf("round-trip: %q", audiofilter.Format(specs))
}
