package audiofilter

import (
	"math"
	"testing"
)

// correlation measures how much of the original waveform survives, which is a
// usable proxy for "can you still follow the words".
func correlation(a, b []int16) float64 {
	n := min(len(a), len(b))
	var num, ea, eb float64
	for i := 0; i < n; i++ {
		x, y := float64(a[i]), float64(b[i])
		num += x * y
		ea += x * x
		eb += y * y
	}
	if ea == 0 || eb == 0 {
		return 0
	}
	return num / math.Sqrt(ea*eb)
}

// TestRoboticKeepsTheOriginalWaveform is the point of this filter. A vocoder
// discards the original excitation and resynthesises, which is why it is hard
// to follow; the comb only colours the signal, so the waveform — and with it
// the intelligibility — largely survives.
func TestRoboticKeepsTheOriginalWaveform(t *testing.T) {
	const rate = 16000
	in := speechLike(rate*2, rate)

	comb, err := newRobotic(rate, defaultPitchHz, defaultDepth, 1)
	if err != nil {
		t.Fatal(err)
	}
	combOut := runStage(t, comb, in, 320)
	combR := correlation(in[rate/2:], combOut[rate/2:])

	voc, err := newVocoder(rate, defaultCarrierHz, defaultBands, 1)
	if err != nil {
		t.Fatal(err)
	}
	vocOut := runStage(t, voc, in, 320)
	vocR := correlation(in[rate/2:], vocOut[rate/2:])

	t.Logf("correlation with the original: robotic(comb) %.2f, vocoder %.2f", combR, vocR)
	if combR < 0.5 {
		t.Errorf("robotic correlation %.2f is too low; the speech is not surviving the effect", combR)
	}
	if combR <= vocR {
		t.Errorf("robotic (%.2f) should preserve more of the original than the vocoder (%.2f)", combR, vocR)
	}
}

// TestRoboticIsNotAPassthrough guards the other side: it has to actually
// sound like something.
func TestRoboticIsNotAPassthrough(t *testing.T) {
	const rate = 16000
	in := speechLike(rate, rate)
	st, err := newRobotic(rate, defaultPitchHz, defaultDepth, 1)
	if err != nil {
		t.Fatal(err)
	}
	out := runStage(t, st, in, 320)
	if r := correlation(in[rate/4:], out[rate/4:]); r > 0.98 {
		t.Errorf("correlation %.3f: the effect is barely doing anything", r)
	} else {
		t.Logf("correlation %.2f: audibly coloured, still recognisable", r)
	}
}

// TestRoboticResonatesAtPitch checks the comb rings at the configured pitch,
// which is what gives the metallic character.
func TestRoboticResonatesAtPitch(t *testing.T) {
	const rate = 16000
	for _, pitch := range []float64{70, 110, 180} {
		st, err := newRobotic(rate, pitch, 0.9, 1)
		if err != nil {
			t.Fatal(err)
		}
		// Broadband input excites every comb resonance.
		rng := uint32(999)
		in := make([]int16, rate*2)
		for i := range in {
			rng ^= rng << 13
			rng ^= rng >> 17
			rng ^= rng << 5
			in[i] = int16(float64(int32(rng)) / 2147483648.0 * 6000)
		}
		out := runStage(t, st, in, 320)
		tail := out[rate:]

		lag := int(math.Round(float64(rate) / pitch))
		var num, e0, e1 float64
		for i := 0; i+lag < len(tail); i++ {
			a, b := float64(tail[i]), float64(tail[i+lag])
			num += a * b
			e0 += a * a
			e1 += b * b
		}
		r := num / math.Sqrt(e0*e1)
		t.Logf("pitch %.0f Hz: periodicity at the comb period = %.2f", pitch, r)
		if r < 0.3 {
			t.Errorf("pitch %.0f Hz: comb is not resonating (r=%.2f)", pitch, r)
		}
	}
}

// TestRoboticIsStable checks the feedback loop cannot run away, including at
// the maximum depth the parameters allow.
func TestRoboticIsStable(t *testing.T) {
	const rate = 16000
	st, err := newRobotic(rate, defaultPitchHz, 0.95, 1)
	if err != nil {
		t.Fatal(err)
	}
	in := speechLike(rate*10, rate) // 10 s, long enough for a runaway to show
	out := runStage(t, st, in, 320)

	first, last := rms(out[:rate]), rms(out[len(out)-rate:])
	t.Logf("depth=0.95 over 10 s: first second RMS %.0f, last second RMS %.0f", first, last)
	var clamped int
	for _, v := range out {
		if v >= 32767 || v <= -32768 {
			clamped++
		}
	}
	if pct := float64(clamped) / float64(len(out)) * 100; pct > 0.1 {
		t.Errorf("%.2f%% of samples clamped: the feedback loop is running away", pct)
	}
}

func TestRoboticMixZeroIsPassthrough(t *testing.T) {
	const rate = 16000
	in := speechLike(rate, rate)
	st, err := newRobotic(rate, defaultPitchHz, defaultDepth, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := runStage(t, st, in, 320)
	for i := range in {
		if out[i] != in[i] {
			t.Fatalf("mix=0 must pass audio through; sample %d changed %d -> %d", i, in[i], out[i])
		}
	}
	t.Log("mix=0 passes audio through untouched")
}

func TestRoboticRejectsBadParams(t *testing.T) {
	for _, c := range []struct {
		name string
		p    Params
	}{
		{"pitch too low", Params{"pitch_hz": 10}},
		{"pitch too high", Params{"pitch_hz": 5000}},
		{"depth negative", Params{"depth": -0.1}},
		{"depth at unity", Params{"depth": 1}},
		{"mix out of range", Params{"mix": 2}},
	} {
		if err := Validate([]Spec{{Type: "robotic", Params: c.p}}); err == nil {
			t.Errorf("%s: expected rejection", c.name)
			continue
		}
		t.Logf("%-18s -> rejected", c.name)
	}
	if err := Validate([]Spec{{Type: "robotic"}}); err != nil {
		t.Errorf("defaults must validate: %v", err)
	}
}

// speechLike builds a voiced signal with formants and an amplitude contour —
// closer to speech than a bare tone, without needing a fixture.
func speechLike(n, rate int) []int16 {
	out := make([]int16, n)
	f0, phase := 130.0, 0.0
	for i := range out {
		t := float64(i) / float64(rate)
		phase += f0 / float64(rate)
		if phase >= 1 {
			phase -= 1
		}
		// Glottal-ish pulse train shaped by three formants.
		pulse := 2*phase - 1
		v := pulse * (0.6*math.Sin(2*math.Pi*700*t) +
			0.3*math.Sin(2*math.Pi*1200*t) +
			0.1*math.Sin(2*math.Pi*2600*t))
		// Syllable-rate envelope.
		env := 0.5 + 0.5*math.Sin(2*math.Pi*3*t)
		out[i] = int16(9000 * v * env)
	}
	return out
}
