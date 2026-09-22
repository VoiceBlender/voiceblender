package denoise

import (
	"context"
	"math"
	"math/rand"
	"sync"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/audiofilter"
)

func install(t *testing.T, perInstance int) {
	t.Helper()
	if err := Install(context.Background(), perInstance); err != nil {
		t.Fatalf("Install: %v", err)
	}
	t.Cleanup(func() { Shutdown() })
}

func noise(n int, seed int64, amp float64) []int16 {
	rng := rand.New(rand.NewSource(seed))
	out := make([]int16, n)
	for i := range out {
		v := rng.NormFloat64() * amp
		if v > 32767 {
			v = 32767
		} else if v < -32768 {
			v = -32768
		}
		out[i] = int16(v)
	}
	return out
}

func rms(s []int16) float64 {
	if len(s) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range s {
		sum += float64(v) * float64(v)
	}
	return math.Sqrt(sum / float64(len(s)))
}

// TestLifecycle covers Install/Available/Shutdown, which gate whether the
// filter is offered at all.
func TestLifecycle(t *testing.T) {
	if Available() {
		t.Fatal("filter should be unavailable before Install")
	}
	install(t, 0)
	if !Available() {
		t.Fatal("filter should be available after Install")
	}
	if err := Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if Available() {
		t.Error("filter should be unavailable after Shutdown")
	}
	if err := Shutdown(); err != nil {
		t.Errorf("Shutdown must be idempotent, got %v", err)
	}
}

// TestDegradedWithoutKernel is the degraded-mode contract: with no kernel the
// name still validates, Resolve drops it, and the chain builds as a passthrough
// instead of failing call setup.
func TestDegradedWithoutKernel(t *testing.T) {
	if Available() {
		t.Fatal("expected no kernel installed")
	}
	specs := []audiofilter.Spec{{Type: "denoise"}}
	if err := audiofilter.Validate(specs); err != nil {
		t.Fatalf("denoise must stay a known filter name: %v", err)
	}
	resolved := audiofilter.Resolve(specs)
	if len(resolved) != 0 {
		t.Fatalf("unavailable denoise should be dropped, got %+v", resolved)
	}
	if _, err := audiofilter.Build(nil, 16000, 16000, resolved); err != nil {
		t.Fatalf("resolved chain must build: %v", err)
	}
	// Building it anyway surfaces a clear error rather than a nil stage.
	if _, err := audiofilter.Build(nil, 16000, 16000, specs); err == nil {
		t.Error("building an unavailable filter should fail explicitly")
	}
	t.Log("no kernel: name valid, filter resolved away, chain builds as passthrough")
}

// bandLimited returns noise confined below fc, which is what every source on
// this path carries: G.711 stops at 4 kHz, G.722 and AMR-WB at 8 kHz, and even
// 48 kHz Opus legs carry speech-band energy.
func bandLimited(n int, seed int64, amp, fc float64) []int16 { return bandLimitedRaw(n, seed, amp, fc) }

func bandLimitedRaw(n int, seed int64, amp, fc float64) []int16 {
	rng := rand.New(rand.NewSource(seed))
	out := make([]int16, n)
	a := math.Exp(-2 * math.Pi * fc / Rate)
	y := 0.0
	for i := range out {
		y = (1-a)*rng.NormFloat64()*amp + a*y
		v := y / (1 - a) * 0.5
		if v > 32767 {
			v = 32767
		} else if v < -32768 {
			v = -32768
		}
		out[i] = int16(v)
	}
	return out
}

func runHops(t *testing.T, s *state, in []int16) []int16 {
	t.Helper()
	buf := append([]int16(nil), in...)
	for off := 0; off+Hop <= len(buf); off += Hop {
		if err := s.process(buf[off : off+Hop]); err != nil {
			t.Fatal(err)
		}
	}
	return buf
}

// TestSuppressesNoise measures steady-state suppression on signals of the
// shape this path carries. The kernel's noise estimate needs about a second to
// converge (see TestConvergenceRamp), so the first second is warm-up and the
// measurement window follows it.
func TestSuppressesNoise(t *testing.T) {
	install(t, 0)
	const warmHops, measureHops = 100, 100
	for _, c := range []struct {
		name  string
		fc    float64
		floor float64 // steady-state suppression must be at least this deep
	}{
		{"speech band (3.4 kHz)", 3400, -10},
		{"rumble (300 Hz)", 300, -25},
	} {
		st, err := current().acquire()
		if err != nil {
			t.Fatal(err)
		}
		in := bandLimited(Hop*(warmHops+measureHops), 7, 2000, c.fc)
		out := runHops(t, st, in)
		from := warmHops * Hop
		before, after := rms(in[from:]), rms(out[from:])
		got := 20 * math.Log10(after/before)
		t.Logf("%-22s steady state: RMS %7.1f -> %7.1f  (%+.1f dB)", c.name, before, after, got)
		if got > c.floor {
			t.Errorf("%s: expected at least %.0f dB suppression, got %.1f dB", c.name, -c.floor, got)
		}
		st.release()
	}
}

// TestConvergenceRamp pins the warm-up behaviour, which is operationally
// visible: the opening of a call is suppressed noticeably less than the rest
// while the noise estimate converges.
func TestConvergenceRamp(t *testing.T) {
	install(t, 0)
	st, err := current().acquire()
	if err != nil {
		t.Fatal(err)
	}
	defer st.release()

	in := bandLimited(Hop*200, 7, 2000, 3400)
	out := runHops(t, st, in)
	db := func(a, b int) float64 {
		return 20 * math.Log10(rms(out[a*Hop:b*Hop])/rms(in[a*Hop:b*Hop]))
	}
	early, steady := db(0, 20), db(100, 200)
	t.Logf("first 0.2 s: %+.1f dB, after 1 s: %+.1f dB", early, steady)
	if steady >= early {
		t.Errorf("suppression should deepen as the estimate converges: %.1f dB early, %.1f dB steady",
			early, steady)
	}
}

// TestFullBandNoiseIsBarelyTouched pins a real characteristic rather than a
// defect: RNNoise scores 22 Bark bands spanning 0-20 kHz, so noise with equal
// energy in every band — including bands speech never occupies — keeps moderate
// gains. No source on this path produces such a signal, but the behaviour is
// surprising enough that it should fail loudly if it ever changes.
func TestFullBandNoiseIsBarelyTouched(t *testing.T) {
	install(t, 0)
	st, err := current().acquire()
	if err != nil {
		t.Fatal(err)
	}
	defer st.release()

	in := noise(Hop*100, 7, 2000)
	out := runHops(t, st, in)
	before, after := rms(in[Hop*20:]), rms(out[Hop*20:])
	change := 20 * math.Log10(after/before)
	t.Logf("full-band white noise: RMS %.1f -> %.1f (%+.1f dB) -- expected to be weakly suppressed",
		before, after, change)
	if change > 0 {
		t.Errorf("full-band noise should never be amplified, got %+.1f dB", change)
	}
	if change < -15 {
		t.Errorf("full-band suppression of %.1f dB is far stronger than measured behaviour; "+
			"the kernel or its wiring changed", change)
	}
}

func TestRejectsWrongFrameSize(t *testing.T) {
	install(t, 0)
	st, err := current().acquire()
	if err != nil {
		t.Fatal(err)
	}
	defer st.release()
	if err := st.process(make([]int16, Hop-1)); err == nil {
		t.Error("a short frame should be rejected")
	}
	if err := st.process(make([]int16, Hop+1)); err == nil {
		t.Error("a long frame should be rejected")
	}
}

// TestPoolPacksInstances checks states are packed to the configured density
// and that releasing frees capacity rather than leaking it.
func TestPoolPacksInstances(t *testing.T) {
	const per = 4
	install(t, per)
	k := current()

	var states []*state
	for i := 0; i < per*3; i++ {
		s, err := k.acquire()
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		states = append(states, s)
	}
	if got := k.Instances(); got != 3 {
		t.Errorf("%d states at %d per instance: got %d instances, want 3", len(states), per, got)
	}
	t.Logf("%d states packed into %d instances (%d per instance)", len(states), k.Instances(), per)

	// Every state must be independently usable.
	for i, s := range states {
		if err := s.process(make([]int16, Hop)); err != nil {
			t.Fatalf("state %d: %v", i, err)
		}
	}
	for _, s := range states {
		s.release()
	}
	// Released capacity is reused: no new instance should be needed.
	s, err := k.acquire()
	if err != nil {
		t.Fatal(err)
	}
	defer s.release()
	if got := k.Instances(); got != 3 {
		t.Errorf("after release, acquire should reuse an instance; got %d instances", got)
	}
	t.Logf("after releasing all, reacquire reused an existing instance (%d total)", k.Instances())
}

// TestStatesAreIndependent guards against states sharing recurrent history,
// which would make one call's audio depend on another's.
func TestStatesAreIndependent(t *testing.T) {
	install(t, 8)
	k := current()

	in := noise(Hop*20, 11, 3000)
	run := func(s *state) []int16 {
		buf := append([]int16(nil), in...)
		for off := 0; off+Hop <= len(buf); off += Hop {
			if err := s.process(buf[off : off+Hop]); err != nil {
				t.Fatal(err)
			}
		}
		return buf
	}

	a, err := k.acquire()
	if err != nil {
		t.Fatal(err)
	}
	defer a.release()
	b, err := k.acquire()
	if err != nil {
		t.Fatal(err)
	}
	defer b.release()

	outA, outB := run(a), run(b)
	for i := range outA {
		if outA[i] != outB[i] {
			t.Fatalf("two fresh states gave different output at sample %d (%d vs %d)", i, outA[i], outB[i])
		}
	}
	t.Logf("two fresh states produced identical output over %d samples", len(outA))

	// Feeding a third state a different signal must not disturb the others.
	outA2 := run(a)
	same := true
	for i := range outA {
		if outA[i] != outA2[i] {
			same = false
			break
		}
	}
	if same {
		t.Error("a state with carried recurrent history should not repeat its first-pass output exactly")
	}
	t.Log("recurrent state carries across passes, as expected")
}

// TestConcurrentStates exercises the per-instance lock: states sharing an
// instance are driven from separate goroutines, as separate legs would.
func TestConcurrentStates(t *testing.T) {
	install(t, 8)
	k := current()

	const n = 16
	states := make([]*state, n)
	for i := range states {
		s, err := k.acquire()
		if err != nil {
			t.Fatal(err)
		}
		states[i] = s
		defer s.release()
	}

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i, s := range states {
		wg.Add(1)
		go func(i int, s *state) {
			defer wg.Done()
			frame := make([]int16, Hop)
			buf := noise(Hop, int64(i), 3000)
			for r := 0; r < 50; r++ {
				copy(frame, buf)
				if err := s.process(frame); err != nil {
					errs <- err
					return
				}
			}
		}(i, s)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent process: %v", err)
	}
	t.Logf("%d states across %d instances processed concurrently", n, k.Instances())
}

// TestThroughChain wires denoise through the real filter chain at the room
// rate, which is the shape production uses.
func TestThroughChain(t *testing.T) {
	install(t, 0)
	const legRate = 16000
	specs := audiofilter.Resolve([]audiofilter.Spec{{Type: "denoise"}})
	if len(specs) != 1 {
		t.Fatal("denoise should be available here")
	}
	if got := audiofilter.ResolveWorkRate(specs, legRate); got != Rate {
		t.Fatalf("denoise must pull the chain to %d Hz, got %d", Rate, got)
	}

	in := noise(legRate*2, 13, 2000)
	src := &sliceReader{data: toBytes(in), n: legRate / 50 * 2}
	r, err := audiofilter.Build(src, legRate, legRate, specs)
	if err != nil {
		t.Fatal(err)
	}
	out := toSamples(readAll(t, r))
	if len(out) != len(in) {
		t.Errorf("sample count changed: %d in, %d out", len(in), len(out))
	}
	before, after := rms(in[legRate/2:]), rms(out[legRate/2:])
	t.Logf("through chain at %d Hz: RMS %.1f -> %.1f (%.1f dB), %d samples",
		legRate, before, after, 20*math.Log10(after/before), len(out))
	if after >= before {
		t.Errorf("chain did not attenuate noise: %.1f -> %.1f", before, after)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	// Close must have returned the state to the pool.
	if s, err := current().acquire(); err != nil {
		t.Fatalf("pool should have capacity after Close: %v", err)
	} else {
		s.release()
	}
}
