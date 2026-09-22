package audiofilter

import (
	"fmt"
	"math"
)

// Robotic defaults. Unlike a vocoder, this keeps the original excitation and
// formants and only colours them, so the words stay as easy to follow as the
// untreated audio.
const (
	defaultPitchHz = 110
	defaultDepth   = 0.75
)

func init() {
	Register("robotic", Descriptor{
		ValidateParams: func(p Params) error {
			_, _, _, err := roboticParams(p)
			return err
		},
		New: func(rate int, p Params) (Stage, error) {
			pitch, depth, mix, err := roboticParams(p)
			if err != nil {
				return nil, err
			}
			return newRobotic(rate, pitch, depth, mix)
		},
	})
}

func roboticParams(p Params) (pitchHz, depth, mix float64, err error) {
	pitchHz = p.Get("pitch_hz", defaultPitchHz)
	depth = p.Get("depth", defaultDepth)
	mix = p.Get("mix", 1)
	if pitchHz < 50 || pitchHz > 500 {
		return 0, 0, 0, fmt.Errorf("pitch_hz %g out of range (50 to 500)", pitchHz)
	}
	if depth < 0 || depth > 0.95 {
		return 0, 0, 0, fmt.Errorf("depth %g out of range (0 to 0.95)", depth)
	}
	if mix < 0 || mix > 1 {
		return 0, 0, 0, fmt.Errorf("mix %g out of range (0 to 1)", mix)
	}
	return pitchHz, depth, mix, nil
}

// roboticStage is a feedback comb filter: y[n] = x[n] + depth*y[n-D], with the
// delay set to one period of pitch_hz. That resonates at pitch_hz and its
// harmonics, giving a metallic, mechanical timbre — but the speech itself is
// only filtered, never resynthesised, so intelligibility stays close to the
// original. This is the trade against `vocoder`, which sounds more synthetic
// and is markedly harder to follow.
type roboticStage struct {
	buf   []float64
	idx   int
	depth float64
	mix   float64

	// dcBlock sits in the feedback path. Without it each pass accumulates low
	// frequencies and the comb turns boomy, then unstable.
	dcPrevIn, dcPrevOut float64
	dcCoef              float64

	peak        float64
	peakRelease float64
}

func newRobotic(rate int, pitchHz, depth, mix float64) (Stage, error) {
	delay := int(math.Round(float64(rate) / pitchHz))
	if delay < 2 {
		return nil, fmt.Errorf("pitch_hz %g is too high for a %d Hz stream", pitchHz, rate)
	}
	return &roboticStage{
		buf:         make([]float64, delay),
		depth:       depth,
		mix:         mix,
		dcCoef:      1 - 2*math.Pi*120/float64(rate), // ~120 Hz one-pole
		peakRelease: 1 - math.Exp(-1/(0.250*float64(rate))),
	}, nil
}

func (r *roboticStage) Close() error { return nil }

func (r *roboticStage) Process(frame []int16) {
	for i, sample := range frame {
		in := float64(sample)

		delayed := r.buf[r.idx]

		// One-pole DC blocker on the delayed signal.
		hp := delayed - r.dcPrevIn + r.dcCoef*r.dcPrevOut
		r.dcPrevIn, r.dcPrevOut = delayed, hp

		out := in + r.depth*hp
		r.buf[r.idx] = out
		r.idx++
		if r.idx == len(r.buf) {
			r.idx = 0
		}

		// The comb adds gain at every resonance, so a loud talker would clip
		// without this. Instant attack, slow release.
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
