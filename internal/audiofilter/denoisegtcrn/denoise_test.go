package denoisegtcrn

import (
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/audiofilter"
	_ "github.com/VoiceBlender/voiceblender/internal/audiofilter/denoise"
)

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

// TestDegradedWithoutKernel: with no kernel the name still validates, Resolve
// drops it, and the chain builds as a passthrough instead of failing setup.
func TestDegradedWithoutKernel(t *testing.T) {
	if Available() {
		t.Fatal("expected no kernel installed")
	}
	specs := []audiofilter.Spec{{Type: Name}}
	if err := audiofilter.Validate(specs); err != nil {
		t.Fatalf("%s must stay a known filter name: %v", Name, err)
	}
	resolved := audiofilter.Resolve(specs)
	if len(resolved) != 0 {
		t.Fatalf("unavailable filter should be dropped, got %+v", resolved)
	}
	if _, err := audiofilter.Build(nil, 16000, 16000, resolved); err != nil {
		t.Fatalf("resolved chain must build: %v", err)
	}
	if _, err := audiofilter.Build(nil, 16000, 16000, specs); err == nil {
		t.Error("building an unavailable filter should fail explicitly")
	}
}

func TestExclusiveWithDenoise(t *testing.T) {
	err := audiofilter.Validate([]audiofilter.Spec{{Type: "denoise"}, {Type: Name}})
	if err == nil || !strings.Contains(err.Error(), "cannot both be in a chain") {
		t.Fatalf("denoise and %s together must be rejected, got %v", Name, err)
	}
	if err := audiofilter.Validate([]audiofilter.Spec{{Type: Name}, {Type: Name}}); err == nil {
		t.Errorf("%s twice must be rejected", Name)
	}
	if err := audiofilter.Validate([]audiofilter.Spec{{Type: "bandpass"}, {Type: Name}}); err != nil {
		t.Errorf("bandpass,%s: %v", Name, err)
	}
}

func TestFrameSizes(t *testing.T) {
	install(t)
	for rate, want := range map[int]int{8000: 128, 12000: 192, 16000: 256} {
		st, err := current().acquire(rate)
		if err != nil {
			t.Fatalf("%d Hz: %v", rate, err)
		}
		s := &stage{st: st}
		if got := s.FrameSamples(); got != want {
			t.Errorf("%d Hz: frame %d, want %d (16 ms)", rate, got, want)
		}
		s.Close()
	}
}

// TestRejectsUnsupportedRate: the chain never builds a stage off the model's
// rates, but a direct build at one must fail rather than run the wrong tables.
func TestRejectsUnsupportedRate(t *testing.T) {
	install(t)
	for _, rate := range []int{4000, 11025, 24000, 48000} {
		if _, err := newStage(rate, nil); err == nil {
			t.Errorf("%d Hz should be rejected", rate)
		}
	}
}

// TestSuppressesNoise measures steady-state suppression at every native rate,
// after one second of warm-up.
func TestSuppressesNoise(t *testing.T) {
	install(t)
	const floor = -15.0
	for _, rate := range Rates {
		for _, c := range []struct {
			name string
			in   []int16
		}{
			{"speech band", bandLimited(rate*2, rate, 7, 2000, 3400)},
			{"rumble", bandLimited(rate*2, rate, 7, 2000, 300)},
			{"white", noise(rate*2, 7, 2000)},
		} {
			st, err := current().acquire(rate)
			if err != nil {
				t.Fatal(err)
			}
			out := runFrames(t, st, c.in)
			got := 20 * math.Log10(rms(out[rate:])/rms(c.in[rate:]))
			t.Logf("%5d Hz %-12s %+.1f dB", rate, c.name, got)
			if got > floor {
				t.Errorf("%d Hz %s: expected at least %.0f dB suppression, got %.1f dB", rate, c.name, -floor, got)
			}
			st.release()
		}
	}
}

func TestRejectsWrongFrameSize(t *testing.T) {
	install(t)
	st, err := current().acquire(16000)
	if err != nil {
		t.Fatal(err)
	}
	defer st.release()
	if err := st.process(make([]int16, 255)); err == nil {
		t.Error("a short frame should be rejected")
	}
	if err := st.process(make([]int16, 320)); err == nil {
		t.Error("a 20 ms frame should be rejected")
	}
}

// TestStatesAreIndependent guards against states sharing recurrent history.
func TestStatesAreIndependent(t *testing.T) {
	install(t)
	k := current()
	in := noise(256*20, 11, 3000)
	a, err := k.acquire(16000)
	if err != nil {
		t.Fatal(err)
	}
	defer a.release()
	b, err := k.acquire(16000)
	if err != nil {
		t.Fatal(err)
	}
	defer b.release()

	outA, outB := runFrames(t, a, in), runFrames(t, b, in)
	for i := range outA {
		if outA[i] != outB[i] {
			t.Fatalf("two fresh states differ at sample %d (%d vs %d)", i, outA[i], outB[i])
		}
	}
	outA2 := runFrames(t, a, in)
	same := true
	for i := range outA {
		if outA[i] != outA2[i] {
			same = false
			break
		}
	}
	if same {
		t.Error("a state with carried history should not repeat its first-pass output exactly")
	}
}

func TestConcurrentStates(t *testing.T) {
	install(t)
	k := current()
	const n = 16
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		rate := Rates[i%len(Rates)]
		s, err := k.acquire(rate)
		if err != nil {
			t.Fatal(err)
		}
		defer s.release()
		wg.Add(1)
		go func(i int, s *stream) {
			defer wg.Done()
			frame := make([]int16, s.frame)
			buf := noise(s.frame, int64(i), 3000)
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
}

// TestKernelRecyclesStates checks the pool is keyed by rate, reuses states,
// and a recycled state behaves exactly like a fresh one.
func TestKernelRecyclesStates(t *testing.T) {
	install(t)
	k := current()

	s8, err := k.acquire(8000)
	if err != nil {
		t.Fatal(err)
	}
	s8.release()
	_, built := k.Stats()
	s16, err := k.acquire(16000)
	if err != nil {
		t.Fatal(err)
	}
	if s16.frame != 256 {
		t.Fatalf("16 kHz acquire got a %d-sample state; the pool mixed rates", s16.frame)
	}
	if _, after := k.Stats(); after != built {
		t.Errorf("16 kHz acquire should reuse the probe, built %d -> %d", built, after)
	}

	runFrames(t, s16, noise(256*40, 11, 6000))
	s16.release()

	probe := bandLimited(256*40, 16000, 7, 2000, 3400)
	recycled, err := k.acquire(16000)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := k.acquire(16000)
	if err != nil {
		t.Fatal(err)
	}
	outR, outF := runFrames(t, recycled, probe), runFrames(t, fresh, probe)
	recycled.release()
	fresh.release()
	for i := range outR {
		if outR[i] != outF[i] {
			t.Fatalf("recycled state differs from a fresh one at sample %d", i)
		}
	}
	if live, _ := k.Stats(); live != 0 {
		t.Errorf("%d streams still live after releasing all", live)
	}
}

// TestThroughChainNativeRates: at 8 and 16 kHz the filter runs at the room
// rate, so the chain adds no resampling.
func TestThroughChainNativeRates(t *testing.T) {
	install(t)
	for _, rate := range []int{8000, 16000} {
		specs := audiofilter.Resolve([]audiofilter.Spec{{Type: Name}})
		if got := audiofilter.ResolveWorkRate(specs, rate); got != rate {
			t.Fatalf("%d Hz room: chain moved to %d Hz", rate, got)
		}
		in := noise(rate*2, 13, 2000)
		r, err := audiofilter.Build(&sliceReader{data: toBytes(in), n: rate / 50 * 2}, rate, rate, specs)
		if err != nil {
			t.Fatal(err)
		}
		out := toSamples(readAll(t, r))
		if len(out) != len(in) {
			t.Errorf("%d Hz: sample count changed: %d in, %d out", rate, len(in), len(out))
		}
		before, after := rms(in[rate/2:]), rms(out[rate/2:])
		t.Logf("%d Hz chain: %+.1f dB", rate, 20*math.Log10(after/before))
		if after >= before/4 {
			t.Errorf("%d Hz: chain did not attenuate noise: %.1f -> %.1f", rate, before, after)
		}
		r.Close()
	}
	if live, _ := current().Stats(); live != 0 {
		t.Errorf("Close did not return %d states to the pool", live)
	}
}

// TestThroughChain48k: in a 48 kHz room the chain runs the filter at 16 kHz,
// denoises, and returns 48 kHz audio band-limited to 8 kHz.
func TestThroughChain48k(t *testing.T) {
	install(t)
	const rate = 48000
	specs := []audiofilter.Spec{{Type: Name}}
	if got := audiofilter.ResolveWorkRate(specs, rate); got != 16000 {
		t.Fatalf("48 kHz room: chain resolved to %d Hz, want 16000", got)
	}
	in := bandLimited(rate*2, rate, 13, 2000, 3400)
	// A 12 kHz tone sits above the 16 kHz model's Nyquist and must not survive.
	hi := make([]int16, len(in))
	for i := range hi {
		hi[i] = int16(3000 * math.Sin(2*math.Pi*12000*float64(i)/rate))
	}
	for _, c := range []struct {
		name string
		in   []int16
		max  float64
	}{
		{"speech-band noise", in, -15},
		{"12 kHz tone", hi, -30},
	} {
		r, err := audiofilter.Build(&sliceReader{data: toBytes(c.in), n: rate / 50 * 2}, rate, rate, specs)
		if err != nil {
			t.Fatal(err)
		}
		out := toSamples(readAll(t, r))
		r.Close()
		if d := len(out) - len(c.in); d < -1024 || d > 1024 {
			t.Errorf("%s: %d samples in, %d out", c.name, len(c.in), len(out))
		}
		got := 20 * math.Log10(rms(out[rate:])/rms(c.in[rate:]))
		t.Logf("48 kHz room, %-18s %+.1f dB", c.name, got)
		if got > c.max {
			t.Errorf("%s: %+.1f dB, want at most %+.0f dB", c.name, got, c.max)
		}
	}
}

// TestOddRateRoundsUp: a rate between the model's rates runs at the next one
// up rather than losing bandwidth.
func TestOddRateRoundsUp(t *testing.T) {
	install(t)
	specs := []audiofilter.Spec{{Type: Name}}
	if got := audiofilter.ResolveWorkRate(specs, 11025); got != 12000 {
		t.Fatalf("11025 Hz resolved to %d, want 12000", got)
	}
	in := noise(11025*2, 3, 2000)
	r, err := audiofilter.Build(&sliceReader{data: toBytes(in), n: 882}, 11025, 11025, specs)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	out := toSamples(readAll(t, r))
	if len(out) == 0 {
		t.Fatal("no output at 11025 Hz")
	}
}
