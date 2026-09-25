package room

import (
	"io"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/audiofilter"
	"github.com/VoiceBlender/voiceblender/internal/leg"
)

// TestLegFiltersDefaulting covers the three-way distinction the API relies on:
// a leg that chose nothing takes the server default, a leg that chose a chain
// keeps it, and a leg that explicitly chose none is not given the default back.
func TestLegFiltersDefaulting(t *testing.T) {
	def := []audiofilter.Spec{{Type: "gain", Params: audiofilter.Params{"volume": 1}}}
	r := &Room{defaultFilters: def}

	cases := []struct {
		name string
		legs []audiofilter.Spec
		want int
	}{
		{"unset takes the default", nil, len(def)},
		{"explicitly none stays none", []audiofilter.Spec{}, 0},
		{"own chain wins", []audiofilter.Spec{{Type: "bandpass"}}, 1},
	}
	for _, c := range cases {
		l := &mockLeg{id: "l1", filters: c.legs}
		got := r.legFilters(l)
		if len(got) != c.want {
			t.Errorf("%s: got %d filters, want %d (%+v)", c.name, len(got), c.want, got)
			continue
		}
		t.Logf("%-28s -> %d filter(s)", c.name, len(got))
	}

	// An explicitly-empty chain must be distinguishable from unset, or an
	// opt-out would silently get the default back.
	if got := r.legFilters(&mockLeg{id: "l2", filters: []audiofilter.Spec{}}); got == nil || len(got) != 0 {
		t.Errorf("explicit opt-out must yield an empty chain, got %+v", got)
	}
}

// TestIngressReaderFallsBack checks a chain that cannot be built costs
// filtering, never the leg's audio.
func TestIngressReaderFallsBack(t *testing.T) {
	legMgr := leg.NewManager()
	mgr := NewManager(legMgr, newTestBus(), newTestLog())
	r, err := mgr.Create("room-filters", "", 16000)
	if err != nil {
		t.Fatal(err)
	}
	src := io.LimitReader(readerOfZeros{}, 640)

	// An unknown filter cannot build; audio must still flow.
	got := r.ingressReader("leg1", src, 8000, 16000, []audiofilter.Spec{{Type: "nosuchfilter"}})
	if got == nil {
		t.Fatal("ingressReader must always return a reader")
	}
	buf := make([]byte, 320)
	if _, err := got.Read(buf); err != nil && err != io.EOF {
		t.Fatalf("fallback reader should still deliver audio: %v", err)
	}
	t.Log("unbuildable chain fell back to plain resampling; audio still flows")

	// An empty chain still gets a real chain, so filters can be turned on later.
	if r.ingressReader("leg2", src, 16000, 16000, nil) == nil {
		t.Error("empty chain must still return a reader")
	}
	if _, ok := r.LegFilters("leg2"); !ok {
		t.Error("a leg with no filters must still have a chain to change")
	}
}

// TestFiltersCanBeTurnedOnMidCallForAnUnfilteredLeg is the regression: the room
// used to hand a leg with no chain a plain resampler and register nothing, so
// SetLegFilters found nothing to change and enabling denoise on a live call
// failed -- for exactly the legs most likely to want it, the ones that started
// with no processing configured.
func TestFiltersCanBeTurnedOnMidCallForAnUnfilteredLeg(t *testing.T) {
	legMgr := leg.NewManager()
	mgr := NewManager(legMgr, newTestBus(), newTestLog())
	r, err := mgr.Create("room-midcall", "", 16000)
	if err != nil {
		t.Fatal(err)
	}

	l := newMockLeg("bare")
	l.reader = readerOfZeros{}
	l.writer = io.Discard
	r.AddLeg(l)

	got, ok := r.LegFilters(l.ID())
	if !ok {
		t.Fatal("an unfiltered leg in a room must still expose a chain")
	}
	if len(got) != 0 {
		t.Fatalf("expected an empty chain, got %+v", got)
	}

	found, err := r.SetLegFilters(l.ID(), []audiofilter.Spec{{Type: "bandpass"}})
	if err != nil {
		t.Fatalf("SetLegFilters: %v", err)
	}
	if !found {
		t.Fatal("SetLegFilters found no chain on a connected leg that started unfiltered")
	}
	if got, _ := r.LegFilters(l.ID()); len(got) != 1 || got[0].Type != "bandpass" {
		t.Errorf("chain after the change = %+v, want [bandpass]", got)
	}

	// And back off again.
	if _, err := r.SetLegFilters(l.ID(), []audiofilter.Spec{}); err != nil {
		t.Fatalf("clearing: %v", err)
	}
	if got, _ := r.LegFilters(l.ID()); len(got) != 0 {
		t.Errorf("chain after clearing = %+v, want none", got)
	}
	t.Log("a leg that joined with no filters can have them turned on and off mid-call")
}

type readerOfZeros struct{}

func (readerOfZeros) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}
