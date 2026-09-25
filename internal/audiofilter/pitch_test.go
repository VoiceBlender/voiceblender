package audiofilter

import (
	"math"
	"testing"
)

// TestPitchShiftsByTheRequestedInterval is the defining check: the output's
// fundamental must move by the requested number of semitones, and only that.
func TestPitchShiftsByTheRequestedInterval(t *testing.T) {
	const rate = 16000
	const inputHz = 200
	for _, semitones := range []float64{-12, -7, -5, 0, 5, 7, 12} {
		st, err := newPitch(rate, semitones, 1)
		if err != nil {
			t.Fatal(err)
		}
		in := saw(rate*2, rate, inputHz, 8000)
		out := runStage(t, st, in, 320)

		want := inputHz * math.Pow(2, semitones/12)
		got := fundamentalHz(out[rate:], rate)
		cents := 1200 * math.Log2(got/want)
		t.Logf("%+3.0f semitones: %.0f Hz -> %.0f Hz (want %.0f Hz, off by %+.0f cents)",
			semitones, float64(inputHz), got, want, cents)
		// A semitone is 100 cents; 50 cents either way is a quarter tone.
		if math.Abs(cents) > 50 {
			t.Errorf("%+.0f semitones: got %.0f Hz, want %.0f Hz (%.0f cents off)", semitones, got, want, cents)
		}
	}
}

// TestPitchPreservesDuration checks the shifter works on a stream: a pitch
// change must not change how many samples come out, or the leg would drift
// against the mixer clock.
func TestPitchPreservesDuration(t *testing.T) {
	const rate = 16000
	for _, semitones := range []float64{-12, -5, 7, 12} {
		st, err := newPitch(rate, semitones, 1)
		if err != nil {
			t.Fatal(err)
		}
		in := saw(rate, rate, 200, 8000)
		out := runStage(t, st, in, 320)
		if len(out) != len(in) {
			t.Errorf("%+.0f semitones: %d samples in, %d out", semitones, len(in), len(out))
			continue
		}
		t.Logf("%+3.0f semitones: %d samples in, %d out", semitones, len(in), len(out))
	}
}

// TestPitchZeroIsTransparent checks semitones=0 leaves the signal alone apart
// from the delay line's own latency.
func TestPitchZeroIsTransparent(t *testing.T) {
	const rate = 16000
	st, err := newPitch(rate, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	in := speechLike(rate*2, rate)
	out := runStage(t, st, in, 320)
	// The taps sit half a window apart, so compare on level and pitch rather
	// than sample alignment.
	inRMS, outRMS := rms(in[rate:]), rms(out[rate:])
	t.Logf("semitones=0: RMS %.0f -> %.0f (%+.1f dB)", inRMS, outRMS, 20*math.Log10(outRMS/inRMS))
	if math.Abs(20*math.Log10(outRMS/inRMS)) > 3 {
		t.Errorf("semitones=0 changed the level by %.1f dB", 20*math.Log10(outRMS/inRMS))
	}
}

func TestPitchMixZeroIsPassthrough(t *testing.T) {
	const rate = 16000
	in := speechLike(rate, rate)
	st, err := newPitch(rate, -5, 0)
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

// TestPitchDoesNotClip checks the crossfade is equal-power: a naive sum of two
// taps would overshoot by up to 6 dB and clip a loud talker.
func TestPitchDoesNotClip(t *testing.T) {
	const rate = 16000
	st, err := newPitch(rate, -5, 1)
	if err != nil {
		t.Fatal(err)
	}
	in := saw(rate*2, rate, 180, 31000)
	out := runStage(t, st, in, 320)
	var clamped int
	for _, v := range out[rate:] {
		if v >= 32767 || v <= -32768 {
			clamped++
		}
	}
	pct := float64(clamped) / float64(len(out[rate:])) * 100
	t.Logf("near-full-scale input: peak %d, clamped %.3f%%", peakAbs(out[rate:]), pct)
	if pct > 0.5 {
		t.Errorf("%.2f%% of samples clamped", pct)
	}
}

func TestPitchRejectsBadParams(t *testing.T) {
	for _, c := range []struct {
		name string
		p    Params
	}{
		{"too far down", Params{"semitones": -24}},
		{"too far up", Params{"semitones": 24}},
		{"mix out of range", Params{"mix": -1}},
	} {
		if err := Validate([]Spec{{Type: "pitch", Params: c.p}}); err == nil {
			t.Errorf("%s: expected rejection", c.name)
			continue
		}
		t.Logf("%-18s -> rejected", c.name)
	}
	if err := Validate([]Spec{{Type: "pitch"}}); err != nil {
		t.Errorf("defaults must validate: %v", err)
	}
}

// saw builds a sawtooth: harmonically rich, so autocorrelation can find its
// fundamental reliably.
func saw(n, rate int, hz, amp float64) []int16 {
	out := make([]int16, n)
	ph := 0.0
	for i := range out {
		ph += hz / float64(rate)
		if ph >= 1 {
			ph -= 1
		}
		out[i] = int16(amp * (2*ph - 1))
	}
	return out
}
