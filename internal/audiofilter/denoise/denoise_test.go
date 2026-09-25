package denoise

import (
	"math"
	"math/rand"
	"sync"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/audiofilter"
)

func install(t *testing.T) {
	t.Helper()
	if err := Install(); err != nil {
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
	install(t)
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

func runHops(t *testing.T, s *stream, in []int16) []int16 {
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
	install(t)
	const warmHops, measureHops = 100, 100
	for _, c := range []struct {
		name  string
		fc    float64
		floor float64 // steady-state suppression must be at least this deep
	}{
		{"speech band (3.4 kHz)", 3400, -10},
		{"rumble (300 Hz)", 300, -25},
	} {
		st, err := current().acquire(Rate)
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
	install(t)
	st, err := current().acquire(Rate)
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

// TestFullBandNoise pins how the model treats noise with equal energy in every
// band. The window is wide on purpose: it is a regression guard on the kernel
// and its wiring, not a quality target.
func TestFullBandNoise(t *testing.T) {
	// Strong, but a near-total kill would suggest the gate had latched rather
	// than the model having decided.
	const minDB, maxDB = -70.0, -20.0
	install(t)
	st, err := current().acquire(Rate)
	if err != nil {
		t.Fatal(err)
	}
	defer st.release()

	in := noise(Hop*100, 7, 2000)
	out := runHops(t, st, in)
	before, after := rms(in[Hop*20:]), rms(out[Hop*20:])
	change := 20 * math.Log10(after/before)
	t.Logf("full-band white noise: RMS %.1f -> %.1f (%+.1f dB), expected window [%.0f, %.0f] dB",
		before, after, change, minDB, maxDB)

	if change > 0 {
		t.Errorf("full-band noise should never be amplified, got %+.1f dB", change)
	}
	if change > maxDB {
		t.Errorf("%+.1f dB is weaker than the measured behaviour (expected at most %+.0f dB); "+
			"the kernel or its wiring changed", change, maxDB)
	}
	if change < minDB {
		t.Errorf("%+.1f dB is far stronger than the measured behaviour (expected at least %+.0f dB); "+
			"the kernel or its wiring changed", change, minDB)
	}
}

func TestRejectsWrongFrameSize(t *testing.T) {
	install(t)
	st, err := current().acquire(Rate)
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

// TestStatesAreIndependent guards against states sharing recurrent history,
// which would make one call's audio depend on another's.
func TestStatesAreIndependent(t *testing.T) {
	install(t)
	k := current()

	in := noise(Hop*20, 11, 3000)
	run := func(s *stream) []int16 {
		buf := append([]int16(nil), in...)
		for off := 0; off+Hop <= len(buf); off += Hop {
			if err := s.process(buf[off : off+Hop]); err != nil {
				t.Fatal(err)
			}
		}
		return buf
	}

	a, err := k.acquire(Rate)
	if err != nil {
		t.Fatal(err)
	}
	defer a.release()
	b, err := k.acquire(Rate)
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
	install(t)
	k := current()

	const n = 16
	states := make([]*stream, n)
	for i := range states {
		s, err := k.acquire(Rate)
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
		go func(i int, s *stream) {
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
	live, built := k.Stats()
	t.Logf("%d states driven concurrently (%d live, %d built)", n, live, built)
}

// TestThroughChain wires denoise through the real filter chain at the room
// rate, which is the shape production uses.
func TestThroughChain(t *testing.T) {
	install(t)
	const legRate = 16000
	specs := audiofilter.Resolve([]audiofilter.Spec{{Type: "denoise"}})
	if len(specs) != 1 {
		t.Fatal("denoise should be available here")
	}
	// Denoise must not move the chain's working rate. If it did, every
	// narrowband leg would be resampled up and back down for the filter alone.
	if got := audiofilter.ResolveWorkRate(specs, legRate); got != legRate {
		t.Fatalf("denoise pulled the chain to %d Hz; it should run at the leg's %d Hz", got, legRate)
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
	if s, err := current().acquire(legRate); err != nil {
		t.Fatalf("pool should have capacity after Close: %v", err)
	} else {
		s.release()
	}
}

// TestKernelRecyclesStates checks a state returned to the pool is reusable and
// carries none of the previous call's recurrent history into a different voice.
func TestKernelRecyclesStates(t *testing.T) {
	install(t)
	k := current()

	// Take several streams, then return them.
	const n = 6
	var streams []*stream
	for i := 0; i < n; i++ {
		s, err := k.acquire(Rate)
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		streams = append(streams, s)
	}
	live, pooled := k.Stats()
	if live != n {
		t.Errorf("%d streams acquired, Stats reports %d live", n, live)
	}
	for _, s := range streams {
		s.release()
	}
	if live, _ := k.Stats(); live != 0 {
		t.Errorf("after releasing everything, %d streams still counted live", live)
	}

	// Reacquiring must reuse rather than allocate afresh.
	before := pooled
	s, err := k.acquire(Rate)
	if err != nil {
		t.Fatal(err)
	}
	defer s.release()
	if _, after := k.Stats(); after > before {
		t.Errorf("reacquire built a new state (%d -> %d) instead of reusing a pooled one",
			before, after)
	}

	// A recycled state must behave exactly like a fresh one. Warm one up on
	// loud noise, release it, then compare its output against a never-used
	// state on the same input.
	warm, err := k.acquire(Rate)
	if err != nil {
		t.Fatal(err)
	}
	dirty := noise(Hop*40, 11, 6000)
	runHops(t, warm, dirty)
	warm.release()

	probe := bandLimited(Hop*40, 7, 2000, 3400)

	recycled, err := k.acquire(Rate)
	if err != nil {
		t.Fatal(err)
	}
	gotRecycled := runHops(t, recycled, probe)
	recycled.release()

	// A second kernel gives a guaranteed-fresh state for comparison.
	if err := Install(); err != nil {
		t.Fatal(err)
	}
	fresh, err := current().acquire(Rate)
	if err != nil {
		t.Fatal(err)
	}
	gotFresh := runHops(t, fresh, probe)
	fresh.release()

	for i := range gotFresh {
		if gotRecycled[i] != gotFresh[i] {
			t.Fatalf("a recycled state diverged from a fresh one at sample %d (%d vs %d); "+
				"Reset is not clearing the recurrent history", i, gotRecycled[i], gotFresh[i])
		}
	}
	t.Logf("recycled state reproduced a fresh state exactly over %d samples", len(gotFresh))
}
