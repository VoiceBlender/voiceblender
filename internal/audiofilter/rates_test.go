package audiofilter

import (
	"strings"
	"testing"
)

// rateStage records the rate it was built at and checks every frame is the
// 16 ms hop at that rate, the shape of a rate-limited model such as GTCRN.
type rateStage struct {
	rate  int
	bad   *int
	calls *int
}

func (s *rateStage) FrameSamples() int { return s.rate * 16 / 1000 }
func (s *rateStage) Close() error      { return nil }
func (s *rateStage) Process(frame []int16) {
	*s.calls++
	if len(frame) != s.FrameSamples() {
		*s.bad++
	}
}

func registerRateLimited(t *testing.T, name, group string, rates []int, built *[]int) {
	t.Helper()
	var calls, bad int
	Register(name, Descriptor{Unique: true, Group: group, Rates: rates,
		New: func(rate int, _ Params) (Stage, error) {
			*built = append(*built, rate)
			return &rateStage{rate: rate, calls: &calls, bad: &bad}, nil
		}})
	t.Cleanup(func() { Unregister(name) })
}

func TestFitRate(t *testing.T) {
	rates := []int{8000, 12000, 16000}
	cases := []struct{ work, want int }{
		{8000, 8000}, {12000, 12000}, {16000, 16000},
		{11025, 12000}, {9000, 12000}, {4000, 8000},
		{22050, 16000}, {24000, 16000}, {44100, 16000}, {48000, 16000},
	}
	for _, c := range cases {
		if got := fitRate(c.work, rates); got != c.want {
			t.Errorf("fitRate(%d) = %d, want %d", c.work, got, c.want)
		}
	}
	if got := fitRate(48000, nil); got != 48000 {
		t.Errorf("no rate limits must leave the rate alone, got %d", got)
	}
}

// TestResolveWorkRateFitsSupportedRates covers a filter that runs only at a
// few rates: the chain moves to the closest one and resamples around it.
func TestResolveWorkRateFitsSupportedRates(t *testing.T) {
	var built []int
	registerRateLimited(t, "narrowtest", "", []int{8000, 12000, 16000}, &built)

	cases := []struct {
		list    string
		dstRate int
		want    int
	}{
		{"narrowtest", 8000, 8000},
		{"narrowtest", 16000, 16000},
		{"narrowtest", 48000, 16000},
		{"narrowtest", 24000, 16000},
		{"narrowtest", 11025, 12000},
		{"bandpass,narrowtest", 48000, 16000},
		{"bandpass", 48000, 48000},
	}
	for _, c := range cases {
		specs, err := Parse(c.list)
		if err != nil {
			t.Fatalf("%s: %v", c.list, err)
		}
		if got := ResolveWorkRate(specs, c.dstRate); got != c.want {
			t.Errorf("%-20s dst=%d: got %d, want %d", c.list, c.dstRate, got, c.want)
		}
	}
}

// TestRateLimitedChainResamplesOnce builds a 48 kHz chain around a 16 kHz-only
// filter: the stage is built at 16 kHz, sees whole 16 ms frames, and the chain
// still hands back 48 kHz audio of the right length.
func TestRateLimitedChainResamplesOnce(t *testing.T) {
	var built []int
	registerRateLimited(t, "narrowtest", "", []int{8000, 12000, 16000}, &built)

	c, err := buildChain(48000, 48000, []Spec{{Type: "narrowtest"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.work != 16000 || c.hop != 256 {
		t.Fatalf("work=%d hop=%d, want 16000/256", c.work, c.hop)
	}
	if c.inRS == nil || c.outRS == nil {
		t.Fatal("a 48 kHz chain around a 16 kHz stage needs both resamplers")
	}

	const secs = 2
	in := tone(48000*secs, 48000, 440, 8000)
	r, err := Build(&chunkSource{data: toBytes(in), n: 1920}, 48000, 48000, []Spec{{Type: "narrowtest"}})
	if err != nil {
		t.Fatal(err)
	}
	out := toSamples(readAll(t, r, 1920))
	if diff := len(out) - len(in); diff < -1024 || diff > 1024 {
		t.Errorf("output %d samples for %d in", len(out), len(in))
	}
	st := r.c.stages[0].(*rateStage)
	if *st.bad != 0 {
		t.Errorf("%d of %d frames were not 256 samples", *st.bad, *st.calls)
	}
	if built[len(built)-1] != 16000 {
		t.Errorf("stage built at %d Hz, want 16000", built[len(built)-1])
	}
	t.Logf("48 kHz in/out, stage at 16 kHz: %d frames, %d samples out", *st.calls, len(out))
}

func TestValidateGroup(t *testing.T) {
	var built []int
	registerRateLimited(t, "grp_a", "g", nil, &built)
	registerRateLimited(t, "grp_b", "g", nil, &built)

	err := Validate([]Spec{{Type: "grp_a"}, {Type: "grp_b"}})
	if err == nil || !strings.Contains(err.Error(), "cannot both be in a chain") {
		t.Fatalf("two filters from one group must be rejected, got %v", err)
	}
	if err := Validate([]Spec{{Type: "grp_a"}, {Type: "bandpass"}}); err != nil {
		t.Errorf("a grouped filter alongside an ungrouped one: %v", err)
	}
}

func TestValidateRejectsIncompatibleRates(t *testing.T) {
	var built []int
	registerRateLimited(t, "only16", "", []int{16000}, &built)
	registerRateLimited(t, "only8", "", []int{8000}, &built)
	Register("needs48", Descriptor{RequiredRate: 48000,
		New: func(int, Params) (Stage, error) { return &gainStage{gain: 1}, nil }})
	defer Unregister("needs48")

	if err := Validate([]Spec{{Type: "only16"}, {Type: "only8"}}); err == nil {
		t.Error("filters with no common rate must be rejected")
	}
	if err := Validate([]Spec{{Type: "only16"}, {Type: "needs48"}}); err == nil {
		t.Error("a required rate outside another filter's rates must be rejected")
	}
	if err := Validate([]Spec{{Type: "only16"}}); err != nil {
		t.Errorf("single rate-limited filter: %v", err)
	}
}

// TestSetFiltersOntoRateLimitedFilter swaps a 48 kHz chain onto a 16 kHz-only
// filter mid-stream: the working rate drops and audio keeps flowing.
func TestSetFiltersOntoRateLimitedFilter(t *testing.T) {
	var built []int
	registerRateLimited(t, "narrowtest", "", []int{8000, 12000, 16000}, &built)

	const rate = 48000
	src := &chunkSource{data: toBytes(tone(rate*4, rate, 440, 8000)), n: 1920}
	r, err := Build(src, rate, rate, []Spec{{Type: "bandpass"}})
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1920)
	for i := 0; i < 25; i++ {
		if _, err := r.Read(buf); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.SetFilters([]Spec{{Type: "narrowtest"}}); err != nil {
		t.Fatal(err)
	}
	var total int
	for i := 0; i < 50; i++ {
		n, err := r.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		total += n
	}
	if r.c.work != 16000 {
		t.Errorf("work rate after swap = %d, want 16000", r.c.work)
	}
	if total == 0 {
		t.Fatal("no audio after the swap")
	}
}
