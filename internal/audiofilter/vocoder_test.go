package audiofilter

import (
	"math"
	"testing"
)

// fundamentalHz estimates the pitch of a signal by autocorrelation, which is
// how we check the vocoder imposes the carrier's pitch rather than passing the
// input's through.
func fundamentalHz(s []int16, rate int) float64 {
	// Mean removal plus energy normalisation: a raw correlation sum grows with
	// lag for any signal with low-frequency content, which makes it pick the
	// longest lag in the search range every time.
	x := make([]float64, len(s))
	var mean float64
	for _, v := range s {
		mean += float64(v)
	}
	mean /= float64(len(s))
	for i, v := range s {
		x[i] = float64(v) - mean
	}

	minLag, maxLag := rate/400, rate/50 // search 50-400 Hz
	r := make([]float64, maxLag+1)
	best := -math.MaxFloat64
	for lag := minLag; lag <= maxLag; lag++ {
		var num, e0, e1 float64
		for i := 0; i+lag < len(x); i++ {
			num += x[i] * x[i+lag]
			e0 += x[i] * x[i]
			e1 += x[i+lag] * x[i+lag]
		}
		if den := math.Sqrt(e0 * e1); den != 0 {
			r[lag] = num / den
			if r[lag] > best {
				best = r[lag]
			}
		}
	}
	// Autocorrelation peaks at every multiple of the true period, and rounding
	// can make r(2T) edge past r(T). Taking the global maximum therefore lands
	// an octave low; take the shortest lag that is essentially as strong.
	for lag := minLag; lag <= maxLag; lag++ {
		if r[lag] >= best*0.95 {
			return float64(rate) / float64(lag)
		}
	}
	return 0
}

func runStage(t *testing.T, st Stage, in []int16, frameLen int) []int16 {
	t.Helper()
	out := append([]int16(nil), in...)
	for off := 0; off+frameLen <= len(out); off += frameLen {
		st.Process(out[off : off+frameLen])
	}
	return out
}

// TestVocoderImposesCarrierPitch is the defining property of a vocoder: the
// output's pitch comes from the carrier, so two different input pitches produce
// the same output pitch. A ring modulator or a passthrough would fail this.
func TestVocoderImposesCarrierPitch(t *testing.T) {
	const rate = 16000
	for _, carrierHz := range []float64{110, 220} {
		var pitches []float64
		for _, inputHz := range []float64{150, 300} {
			st, err := newVocoder(rate, carrierHz, defaultBands, 1)
			if err != nil {
				t.Fatal(err)
			}
			// A sawtooth input is harmonically rich, like voiced speech.
			in := make([]int16, rate*2)
			phase := 0.0
			for i := range in {
				phase += inputHz / rate
				if phase >= 1 {
					phase -= 1
				}
				in[i] = int16(8000 * (2*phase - 1))
			}
			out := runStage(t, st, in, 320)
			// Skip the settling transient and the level-matching ramp.
			got := fundamentalHz(out[rate:], rate)
			pitches = append(pitches, got)
			t.Logf("carrier %.0f Hz, input %.0f Hz -> output %.0f Hz", carrierHz, inputHz, got)
			if math.Abs(got-carrierHz) > carrierHz*0.15 {
				t.Errorf("carrier %.0f Hz with %.0f Hz input: output pitch %.0f Hz, want ~%.0f Hz",
					carrierHz, inputHz, got, carrierHz)
			}
		}
		if math.Abs(pitches[0]-pitches[1]) > carrierHz*0.15 {
			t.Errorf("carrier %.0f Hz: two input pitches gave different output pitches %.0f and %.0f Hz",
				carrierHz, pitches[0], pitches[1])
		}
	}
}

// TestVocoderTracksInputLevel checks the makeup gain: without it the output
// level would swing with voice content and band count.
func TestVocoderTracksInputLevel(t *testing.T) {
	const rate = 16000
	for _, amp := range []float64{1000, 8000} {
		st, err := newVocoder(rate, defaultCarrierHz, defaultBands, 1)
		if err != nil {
			t.Fatal(err)
		}
		in := make([]int16, rate*2)
		phase := 0.0
		for i := range in {
			phase += 200.0 / rate
			if phase >= 1 {
				phase -= 1
			}
			in[i] = int16(amp * (2*phase - 1))
		}
		out := runStage(t, st, in, 320)
		before, after := rms(in[rate:]), rms(out[rate:])
		ratio := 20 * math.Log10(after/before)
		t.Logf("input RMS %.0f -> output RMS %.0f  (%+.1f dB)", before, after, ratio)
		if math.Abs(ratio) > 6 {
			t.Errorf("output level drifted %.1f dB from the input at amplitude %.0f", ratio, amp)
		}
	}
}

// TestVocoderSilenceStaysSilent guards against the carrier leaking through when
// nobody is speaking — an always-on buzz would be unusable on a call.
func TestVocoderSilenceStaysSilent(t *testing.T) {
	const rate = 16000
	st, err := newVocoder(rate, defaultCarrierHz, defaultBands, 1)
	if err != nil {
		t.Fatal(err)
	}
	out := runStage(t, st, make([]int16, rate), 320)
	if peak := peakAbs(out); peak > 16 {
		t.Errorf("silence produced a peak of %d; the carrier is leaking", peak)
	}
	t.Log("silent input stays silent: no carrier bleed")
}

func peakAbs(s []int16) int {
	m := 0
	for _, v := range s {
		a := int(v)
		if a < 0 {
			a = -a
		}
		if a > m {
			m = a
		}
	}
	return m
}

// TestVocoderMixBlends checks mix=0 is a passthrough, so the effect can be
// dialled back without removing it from the chain.
func TestVocoderMixBlends(t *testing.T) {
	const rate = 16000
	in := tone(rate, rate, 300, 6000)
	st, err := newVocoder(rate, defaultCarrierHz, defaultBands, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := runStage(t, st, in, 320)
	for i := range in {
		if out[i] != in[i] {
			t.Fatalf("mix=0 must be a passthrough; sample %d changed %d -> %d", i, in[i], out[i])
		}
	}
	t.Log("mix=0 passes audio through untouched")
}

func TestVocoderRejectsBadParams(t *testing.T) {
	for _, c := range []struct {
		name string
		p    Params
	}{
		{"carrier too low", Params{"carrier_hz": 10}},
		{"carrier too high", Params{"carrier_hz": 5000}},
		{"too few bands", Params{"bands": 1}},
		{"too many bands", Params{"bands": 100}},
		{"mix out of range", Params{"mix": 2}},
	} {
		if err := Validate([]Spec{{Type: "vocoder", Params: c.p}}); err == nil {
			t.Errorf("%s: expected rejection", c.name)
			continue
		}
		t.Logf("%-18s -> rejected", c.name)
	}
	if err := Validate([]Spec{{Type: "vocoder"}}); err != nil {
		t.Errorf("defaults must validate: %v", err)
	}
}

// TestVocoderLimitsLoudInput guards the limiter. Vocoder output is markedly
// spikier than its input, so matching RMS alone lets a loud caller slam into
// the int16 clamp — which on a buzzy signal sounds like breakup, not loudness.
func TestVocoderLimitsLoudInput(t *testing.T) {
	const rate = 16000
	st, err := newVocoder(rate, defaultCarrierHz, defaultBands, 1)
	if err != nil {
		t.Fatal(err)
	}
	// A near-full-scale talker.
	in := make([]int16, rate*2)
	phase := 0.0
	for i := range in {
		phase += 180.0 / rate
		if phase >= 1 {
			phase -= 1
		}
		in[i] = int16(31000 * (2*phase - 1))
	}
	out := runStage(t, st, in, 320)

	tail := out[rate:]
	var clamped int
	for _, v := range tail {
		if v >= 32767 || v <= -32768 {
			clamped++
		}
	}
	pct := float64(clamped) / float64(len(tail)) * 100
	t.Logf("near-full-scale input: peak %d, clamped samples %.3f%%", peakAbs(tail), pct)
	if pct > 0.1 {
		t.Errorf("%.2f%% of samples hit the clamp; the limiter is not holding", pct)
	}
}

// magnitudeAt evaluates a biquad's frequency response, so band coverage can be
// measured directly rather than inferred from output level — the level-matching
// makeup gain compensates for weak bands and hides gaps from any RMS-based check.
func magnitudeAt(f *biquad, hz, rate float64) float64 {
	w := 2 * math.Pi * hz / rate
	cos1, sin1 := math.Cos(w), math.Sin(w)
	cos2, sin2 := math.Cos(2*w), math.Sin(2*w)
	nRe := f.b0 + f.b1*cos1 + f.b2*cos2
	nIm := -(f.b1*sin1 + f.b2*sin2)
	dRe := 1 + f.a1*cos1 + f.a2*cos2
	dIm := -(f.a1*sin1 + f.a2*sin2)
	return math.Hypot(nRe, nIm) / math.Hypot(dRe, dIm)
}

// TestVocoderBankIsFlat bounds the analysis bank's passband ripple. Q is
// derived from the band spacing so neighbouring bands meet; this is a
// regression guard on that layout rather than a sensitive one — biquad skirts
// overlap enough that a moderately wrong Q still sums to a usable response, so
// it catches an egregious Q, not a marginal one.
func TestVocoderBankIsFlat(t *testing.T) {
	const rate = 16000
	st, err := newVocoder(rate, defaultCarrierHz, defaultBands, 1)
	if err != nil {
		t.Fatal(err)
	}
	rs := st.(*vocoderStage)

	lo, hi := math.MaxFloat64, 0.0
	for hz := 300.0; hz <= 3400; hz += 10 {
		var sum float64
		for i := range rs.bands {
			sum += magnitudeAt(rs.bands[i].analysis, hz, rate)
		}
		lo, hi = math.Min(lo, sum), math.Max(hi, sum)
	}
	ripple := 20 * math.Log10(hi/lo)
	t.Logf("analysis bank across 300-3400 Hz: min=%.2f max=%.2f ripple=%.1f dB", lo, hi, ripple)
	if ripple > 5 {
		t.Errorf("passband ripple %.1f dB is too high; the band layout is leaving holes", ripple)
	}
	if lo < 1.5 {
		t.Errorf("weakest summed response %.2f is too low; bands are too narrow for their spacing", lo)
	}
}

// TestVocoderPassesUnvoicedSounds covers fricatives. A purely periodic carrier
// cannot represent "s" or "f", so those turn to buzz; the carrier crossfades to
// noise when the input's energy sits above the voicing split.
func TestVocoderPassesUnvoicedSounds(t *testing.T) {
	const rate = 16000
	st, err := newVocoder(rate, defaultCarrierHz, defaultBands, 1)
	if err != nil {
		t.Fatal(err)
	}
	// High-band noise stands in for a sustained "sss".
	rng := uint32(12345)
	in := make([]int16, rate*2)
	hp := highpass(2500, rate)
	for i := range in {
		rng ^= rng << 13
		rng ^= rng >> 17
		rng ^= rng << 5
		in[i] = int16(hp.run(float64(int32(rng)) / 2147483648.0 * 8000))
	}
	out := runStage(t, st, in, 320)

	// A buzzy carrier would leave a strong periodic peak at the carrier pitch.
	// Noise should not, so correlation at the carrier lag must stay low.
	tail := out[rate:]
	lag := rate / int(defaultCarrierHz)
	var num, e0, e1 float64
	for i := 0; i+lag < len(tail); i++ {
		a, b := float64(tail[i]), float64(tail[i+lag])
		num += a * b
		e0 += a * a
		e1 += b * b
	}
	r := num / math.Sqrt(e0*e1)
	t.Logf("unvoiced input: periodicity at the carrier = %.3f (low means it stayed noise-like)", r)
	if r > 0.5 {
		t.Errorf("unvoiced input came out periodic (r=%.2f); fricatives are being buzzed", r)
	}
	if rms(tail) < rms(in[rate:])*0.25 {
		t.Errorf("unvoiced input was largely swallowed: %.0f -> %.0f RMS", rms(in[rate:]), rms(tail))
	}
}
