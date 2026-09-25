package audiofilter

import (
	"fmt"
	"math"
)

func init() {
	Register("bandpass", Descriptor{
		Corrective: true,
		ValidateParams: func(p Params) error {
			_, _, err := bandpassBand(p)
			return err
		},
		New: func(rate int, p Params) (Stage, error) {
			return newBandpass(rate, p.Get("low_hz", 300), p.Get("high_hz", 3400))
		},
	})
}

// biquad is a direct-form-I section. Its state persists across frames so the
// response is continuous over block boundaries; a per-frame rebuild would emit
// the filter's zero history as every frame's leading samples.
type biquad struct {
	b0, b1, b2, a1, a2 float64
	x1, x2, y1, y2     float64
}

func (f *biquad) run(x float64) float64 {
	y := f.b0*x + f.b1*f.x1 + f.b2*f.x2 - f.a1*f.y1 - f.a2*f.y2
	f.x2, f.x1 = f.x1, x
	f.y2, f.y1 = f.y1, y
	return y
}

// Butterworth sections, Q = 1/sqrt(2).
func lowpass(fc, rate float64) *biquad {
	w := 2 * math.Pi * fc / rate
	c, s := math.Cos(w), math.Sin(w)
	alpha := s / math.Sqrt2
	a0 := 1 + alpha
	return &biquad{
		b0: (1 - c) / 2 / a0, b1: (1 - c) / a0, b2: (1 - c) / 2 / a0,
		a1: -2 * c / a0, a2: (1 - alpha) / a0,
	}
}

func highpass(fc, rate float64) *biquad {
	w := 2 * math.Pi * fc / rate
	c, s := math.Cos(w), math.Sin(w)
	alpha := s / math.Sqrt2
	a0 := 1 + alpha
	return &biquad{
		b0: (1 + c) / 2 / a0, b1: -(1 + c) / a0, b2: (1 + c) / 2 / a0,
		a1: -2 * c / a0, a2: (1 - alpha) / a0,
	}
}

type bandpassStage struct {
	hp, lp *biquad
}

// bandpassBand reads and checks the band, independent of sample rate, so the
// same rules apply at request-validation time and at build time.
func bandpassBand(p Params) (lowHz, highHz float64, err error) {
	lowHz, highHz = p.Get("low_hz", 300), p.Get("high_hz", 3400)
	if lowHz <= 0 {
		return 0, 0, fmt.Errorf("low_hz must be positive, got %g", lowHz)
	}
	if highHz <= lowHz {
		return 0, 0, fmt.Errorf("high_hz (%g) must exceed low_hz (%g)", highHz, lowHz)
	}
	return lowHz, highHz, nil
}

func newBandpass(rate int, lowHz, highHz float64) (Stage, error) {
	if lowHz <= 0 {
		return nil, fmt.Errorf("low_hz must be positive, got %g", lowHz)
	}
	if highHz <= lowHz {
		return nil, fmt.Errorf("high_hz (%g) must exceed low_hz (%g)", highHz, lowHz)
	}
	// Clamping beats silently building a filter whose corner sits above Nyquist.
	if maxHz := float64(rate)/2 - 100; highHz > maxHz {
		highHz = maxHz
	}
	if highHz <= lowHz {
		return nil, fmt.Errorf("band %g-%g Hz does not fit at %d Hz", lowHz, highHz, rate)
	}
	return &bandpassStage{hp: highpass(lowHz, float64(rate)), lp: lowpass(highHz, float64(rate))}, nil
}

func (b *bandpassStage) Close() error { return nil }

func (b *bandpassStage) Process(frame []int16) {
	for i, s := range frame {
		v := b.lp.run(b.hp.run(float64(s)))
		if v > 32767 {
			v = 32767
		} else if v < -32768 {
			v = -32768
		}
		frame[i] = int16(v)
	}
}
