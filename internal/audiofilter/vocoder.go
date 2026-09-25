package audiofilter

import (
	"fmt"
	"math"
)

// Vocoder defaults. The carrier sets the perceived pitch; band count
// trades intelligibility (more bands) against the characteristic synthetic
// timbre (fewer bands).
const (
	// fullScale leaves a little headroom below int16 range so the limiter acts
	// before the hard clamp does.
	fullScale = 32000

	// voicingSplitHz divides bands that carry vowels from those that carry
	// fricatives, for the voiced/unvoiced carrier decision.
	voicingSplitHz = 2000

	defaultCarrierHz = 110
	defaultBands     = 20
	minBands         = 4
	maxBands         = 32
)

func init() {
	Register("vocoder", Descriptor{
		ValidateParams: func(p Params) error {
			_, _, _, err := vocoderParams(p)
			return err
		},
		New: func(rate int, p Params) (Stage, error) {
			carrier, bands, mix, err := vocoderParams(p)
			if err != nil {
				return nil, err
			}
			return newVocoder(rate, carrier, bands, mix)
		},
	})
}

func vocoderParams(p Params) (carrierHz float64, bands int, mix float64, err error) {
	carrierHz = p.Get("carrier_hz", defaultCarrierHz)
	bands = int(p.Get("bands", defaultBands))
	mix = p.Get("mix", 1)
	if carrierHz < 40 || carrierHz > 400 {
		return 0, 0, 0, fmt.Errorf("carrier_hz %g out of range (40 to 400)", carrierHz)
	}
	if bands < minBands || bands > maxBands {
		return 0, 0, 0, fmt.Errorf("bands %d out of range (%d to %d)", bands, minBands, maxBands)
	}
	if mix < 0 || mix > 1 {
		return 0, 0, 0, fmt.Errorf("mix %g out of range (0 to 1)", mix)
	}
	return carrierHz, bands, mix, nil
}

// bandpassQ is an RBJ constant-peak bandpass. The vocoder needs a much tighter
// Q than the Butterworth sections used by the `bandpass` filter, or adjacent
// bands overlap and the spectral envelope smears into mush.
func bandpassQ(fc, q, rate float64) *biquad {
	w := 2 * math.Pi * fc / rate
	c, s := math.Cos(w), math.Sin(w)
	alpha := s / (2 * q)
	a0 := 1 + alpha
	return &biquad{
		b0: alpha / a0, b1: 0, b2: -alpha / a0,
		a1: -2 * c / a0, a2: (1 - alpha) / a0,
	}
}

// vocoderBand is one analysis/synthesis pair: the analysis filter measures how
// much energy the voice has in this band, and the synthesis filter imposes that
// envelope on the same band of the carrier.
type vocoderBand struct {
	analysis  *biquad
	synthesis *biquad
	env       float64
	// highBand marks bands above the voicing split, whose energy indicates a
	// fricative rather than a vowel.
	highBand bool
}

type vocoderStage struct {
	bands []vocoderBand
	mix   float64

	// Sawtooth carrier. Naive (not band-limited), which aliases above the
	// fundamental — inaudible against the effect itself, and the analysis
	// filters discard most of it anyway.
	phase, phaseInc float64

	// A periodic carrier cannot represent a fricative: "s", "f" and "sh" are
	// noise, so a buzz renders them as mush. Track how voiced the input is and
	// cross-fade the carrier towards noise for the unvoiced parts, which is
	// what makes a vocoder intelligible rather than merely rhythmic.
	voicing float64
	rng     uint32

	attack, release float64

	// Running levels for makeup gain. Band gains alone leave the output level
	// dependent on voice content, so it is matched back to the input.
	inLevel, outLevel float64
	levelCoef         float64

	// peak feeds a limiter. Vocoder output has a markedly higher crest factor
	// than its input (measured ~2.5x), so matching RMS alone still clips a
	// loud caller; clipping a buzzy signal sounds like breakup, not loudness.
	peak        float64
	peakRelease float64
}

func newVocoder(rate int, carrierHz float64, bands int, mix float64) (Stage, error) {
	nyquist := float64(rate) / 2
	lowHz, highHz := 200.0, math.Min(3800, nyquist-200)
	if highHz <= lowHz {
		return nil, fmt.Errorf("sample rate %d is too low for the vocoder band layout", rate)
	}

	s := &vocoderStage{
		mix:         mix,
		phaseInc:    carrierHz / float64(rate),
		attack:      1 - math.Exp(-1/(0.005*float64(rate))), // 5 ms
		release:     1 - math.Exp(-1/(0.030*float64(rate))), // 30 ms
		levelCoef:   1 - math.Exp(-1/(0.100*float64(rate))), // 100 ms
		peakRelease: 1 - math.Exp(-1/(0.250*float64(rate))), // 250 ms
	}

	// Log spacing matches how hearing resolves pitch, so the bands stay
	// perceptually even rather than bunching at the top.
	ratio := math.Pow(highHz/lowHz, 1/float64(bands-1))
	// Derive Q from the spacing so neighbouring bands just touch. A fixed Q
	// leaves the spectrum full of holes at wide spacings — speech information
	// that falls in a gap is simply lost, which is the difference between a
	// vocoder you can understand and one you cannot.
	q := 1 / (ratio - 1)
	s.rng = 0x9E3779B9
	for i := 0; i < bands; i++ {
		fc := lowHz * math.Pow(ratio, float64(i))
		s.bands = append(s.bands, vocoderBand{
			analysis:  bandpassQ(fc, q, float64(rate)),
			synthesis: bandpassQ(fc, q, float64(rate)),
			highBand:  fc >= voicingSplitHz,
		})
	}
	return s, nil
}

func (r *vocoderStage) Close() error { return nil }

func (r *vocoderStage) Process(frame []int16) {
	for i, sample := range frame {
		in := float64(sample)

		r.phase += r.phaseInc
		if r.phase >= 1 {
			r.phase -= 1
		}
		buzz := 2*r.phase - 1

		// xorshift: a per-sample noise source cheap enough for the hot path.
		r.rng ^= r.rng << 13
		r.rng ^= r.rng >> 17
		r.rng ^= r.rng << 5
		hiss := float64(int32(r.rng)) / 2147483648.0

		// Voiced input drives the buzz, unvoiced the hiss.
		carrier := buzz*r.voicing + hiss*(1-r.voicing)

		var out, lowEnergy, highEnergy float64
		for b := range r.bands {
			band := &r.bands[b]
			// Envelope of the voice in this band: rectify, then smooth with a
			// fast attack and slower release so consonants survive.
			mag := math.Abs(band.analysis.run(in))
			coef := r.release
			if mag > band.env {
				coef = r.attack
			}
			band.env += (mag - band.env) * coef
			out += band.synthesis.run(carrier) * band.env
			if band.highBand {
				highEnergy += band.env
			} else {
				lowEnergy += band.env
			}
		}

		// Fricatives put most of their energy above the split. Smoothed so the
		// carrier morphs rather than flickering between buzz and hiss.
		target := 0.0
		if total := lowEnergy + highEnergy; total > 1e-9 {
			target = lowEnergy / total
		}
		r.voicing += (target - r.voicing) * r.attack

		r.inLevel += (math.Abs(in) - r.inLevel) * r.levelCoef
		r.outLevel += (math.Abs(out) - r.outLevel) * r.levelCoef
		if r.outLevel > 1e-6 {
			out *= r.inLevel / r.outLevel
		}

		// Instant attack, slow release: catch the transient, then recover.
		if a := math.Abs(out); a > r.peak {
			r.peak = a
		} else {
			r.peak += (a - r.peak) * r.peakRelease
		}
		if r.peak > fullScale {
			out *= fullScale / r.peak
		}

		v := out*r.mix + in*(1-r.mix)
		if v > 32767 {
			v = 32767
		} else if v < -32768 {
			v = -32768
		}
		frame[i] = int16(v)
	}
}
