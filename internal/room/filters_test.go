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

	// An empty chain is the plain resampling path, not a filtered one.
	if r.ingressReader("leg2", src, 16000, 16000, nil) == nil {
		t.Error("empty chain must still return a reader")
	}
}

type readerOfZeros struct{}

func (readerOfZeros) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}
