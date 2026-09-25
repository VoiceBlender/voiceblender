package audiofilter

import (
	"fmt"
	"math"
)

const (
	// defaultSemitones is a noticeable drop rather than 0: a voice-changing
	// filter that does nothing when you enable it is a poor default, however
	// literal-minded it is.
	defaultSemitones = -5
	maxSemitones     = 12

	// windowMs sets the delay-line length. Too short and the crossfade rate
	// becomes audible as warble; too long and the effect smears transients.
	windowMs = 50
)

func init() {
	Register("pitch", Descriptor{
		ValidateParams: func(p Params) error {
			_, _, err := pitchParams(p)
			return err
		},
		New: func(rate int, p Params) (Stage, error) {
			semitones, mix, err := pitchParams(p)
			if err != nil {
				return nil, err
			}
			return newPitch(rate, semitones, mix)
		},
	})
}

func pitchParams(p Params) (semitones, mix float64, err error) {
	semitones = p.Get("semitones", defaultSemitones)
	mix = p.Get("mix", 1)
	if semitones < -maxSemitones || semitones > maxSemitones {
		return 0, 0, fmt.Errorf("semitones %g out of range (-%d to %d)", semitones, maxSemitones, maxSemitones)
	}
	if mix < 0 || mix > 1 {
		return 0, 0, fmt.Errorf("mix %g out of range (0 to 1)", mix)
	}
	return semitones, mix, nil
}

// pitchStage shifts pitch with a crossfading delay line. A single read pointer
// moving at a different speed from the write pointer resamples the signal —
// which shifts pitch — but it must eventually wrap, and the wrap is a
// discontinuity. Two taps half a window apart, crossfaded with an equal-power
// curve, keep one tap away from its wrap at all times.
//
// This shifts formants along with pitch, so a downward shift sounds like a
// larger speaker rather than the same speaker talking lower. That is the usual
// intent for a voice-changing effect; preserving formants needs a far more
// expensive analysis.
type pitchStage struct {
	buf  []float64
	size int
	w    int

	// delay is the read-to-write distance in samples, drifting at a rate set
	// by the pitch ratio and wrapping within the window.
	delay float64
	drift float64
	mix   float64
}

func newPitch(rate int, semitones, mix float64) (Stage, error) {
	size := rate * windowMs / 1000
	if size < 64 {
		return nil, fmt.Errorf("sample rate %d is too low for pitch shifting", rate)
	}
	// Reading faster than writing raises pitch; the delay shrinks as it does.
	ratio := math.Pow(2, semitones/12)
	return &pitchStage{
		buf:   make([]float64, size),
		size:  size,
		delay: float64(size) / 2,
		drift: 1 - ratio,
		mix:   mix,
	}, nil
}

func (p *pitchStage) Close() error { return nil }

// readAt samples the delay line `d` samples behind the write head, with linear
// interpolation because d is fractional.
func (p *pitchStage) readAt(d float64) float64 {
	pos := float64(p.w) - d
	for pos < 0 {
		pos += float64(p.size)
	}
	i := int(pos)
	frac := pos - float64(i)
	a := p.buf[i%p.size]
	b := p.buf[(i+1)%p.size]
	return a + (b-a)*frac
}

func (p *pitchStage) Process(frame []int16) {
	size := float64(p.size)
	for i, sample := range frame {
		in := float64(sample)
		p.buf[p.w] = in
		p.w++
		if p.w == p.size {
			p.w = 0
		}

		d1 := p.delay
		d2 := math.Mod(d1+size/2, size)
		// Equal-power crossfade: each tap fades out as it approaches its wrap,
		// while the other is at its strongest.
		g1 := math.Sin(math.Pi * d1 / size)
		g2 := math.Sin(math.Pi * d2 / size)
		out := p.readAt(d1)*g1 + p.readAt(d2)*g2

		p.delay += p.drift
		if p.delay >= size {
			p.delay -= size
		} else if p.delay < 0 {
			p.delay += size
		}

		v := out*p.mix + in*(1-p.mix)
		if v > 32767 {
			v = 32767
		} else if v < -32768 {
			v = -32768
		}
		frame[i] = int16(v)
	}
}
