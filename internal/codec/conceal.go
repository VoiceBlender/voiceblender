package codec

import (
	"math"
	"slices"
)

const (
	// Pitch search range and window, in samples at 8 kHz (200 Hz to 66 Hz).
	plcPitchMin = 40
	plcPitchMax = 120
	plcCorrLen  = 160
	plcHistLen  = 3*plcPitchMax + plcPitchMax/4

	plcHoldMs = 10 // the substitute plays at full level until here
	plcMuteMs = 60 // and is silent from here
)

// Concealer fills lost frames of a mono PCM stream by pitch-based waveform
// substitution, after ITU-T G.711 Appendix I. It repeats the last pitch
// period (two periods after 10 ms, three after 20 ms), fades the level from
// 10 ms and is silent from 60 ms.
//
// Unlike Appendix I it adds no delay, so a stream with no loss passes through
// unchanged.
type Concealer struct {
	ms   int     // samples per millisecond
	hist []int16 // the most recent output, real or concealed

	erased int // samples concealed in the current gap, 0 outside one
	pitch  int
	quart  int // a quarter pitch period, the length of each cross-fade
	loops  [3][]float32
	k      int // pitch periods in the loop now playing, 1 to 3
	pos    int
	prev   int // position in loops[k-2] while fading out of it
	fade   int
	offset float32
}

// NewConcealer returns a Concealer for a stream at rate Hz, a multiple of 8000.
func NewConcealer(rate int) *Concealer {
	scale := max(rate/8000, 1)
	return &Concealer{ms: 8 * scale, hist: make([]int16, plcHistLen*scale)}
}

// Reset forgets the signal history and any gap in progress.
func (c *Concealer) Reset() {
	clear(c.hist)
	c.erased = 0
}

// Good records a frame that arrived. If it ends a gap that was still being
// concealed, its start is cross-faded in place with the substitute.
func (c *Concealer) Good(pcm []int16) {
	if c.erased > 0 {
		// What follows silence is left alone, as after any pause.
		if c.erased <= plcMuteMs*c.ms {
			n := min(c.mergeLen(), len(pcm))
			for i := range n {
				w := float32(i+1) / float32(n+1)
				pcm[i] = clampInt16(c.next()*(1-w) + float32(pcm[i])*w)
			}
		}
		c.erased = 0
	}
	c.push(pcm)
}

// Conceal returns n samples to stand in for a lost frame, in dst if it is
// large enough.
func (c *Concealer) Conceal(dst []int16, n int) []int16 {
	out := growInt16(dst, n)
	if c.erased == 0 {
		c.start()
	}
	for i := range out {
		out[i] = clampInt16(c.next())
	}
	c.push(out)
	return out
}

// mergeLen is the cross-fade at the end of a gap: 4 ms, 4 ms more for each
// further 10 ms lost, at most 10 ms.
func (c *Concealer) mergeLen() int {
	return min(4+4*((c.erased-1)/(10*c.ms)), 10) * c.ms
}

func (c *Concealer) push(pcm []int16) {
	h := c.hist
	if len(pcm) >= len(h) {
		copy(h, pcm[len(pcm)-len(h):])
		return
	}
	copy(h, h[len(pcm):])
	copy(h[len(h)-len(pcm):], pcm)
}

func (c *Concealer) start() {
	// Silence has nothing to repeat: the gap starts out past the fade.
	if !slices.ContainsFunc(c.hist, func(s int16) bool { return s != 0 }) {
		c.erased = plcMuteMs*c.ms + 1
		return
	}
	c.pitch = c.findPitch()
	c.quart = c.pitch / 4
	for i := range c.loops {
		c.loops[i] = c.buildLoop(c.loops[i], i+1)
	}
	c.k, c.pos, c.fade = 1, 0, 0
	// The substitute starts one period back. Carrying the difference to the
	// last real sample, and letting it die away, joins the two with no step.
	n := len(c.hist)
	c.offset = float32(c.hist[n-1]) - float32(c.hist[n-1-c.pitch])
}

// buildLoop copies the last k pitch periods of history and cross-fades their
// end into the samples just before their start, so the loop repeats with no
// step.
func (c *Concealer) buildLoop(dst []float32, k int) []float32 {
	n, q := len(c.hist), c.quart
	l := k * c.pitch
	if cap(dst) < l {
		dst = make([]float32, l)
	}
	dst = dst[:l]
	for i, s := range c.hist[n-l:] {
		dst[i] = float32(s)
	}
	for i := range q {
		a := float32(i+1) / float32(q)
		dst[l-q+i] = float32(c.hist[n-q+i])*(1-a) + float32(c.hist[n-l-q+i])*a
	}
	return dst
}

// next returns the next sample of the substitute, with the level fade applied.
func (c *Concealer) next() float32 {
	t := c.erased
	c.erased++
	if t >= plcMuteMs*c.ms {
		return 0
	}

	// A longer loop at 10 ms and at 20 ms keeps the repeat from buzzing.
	if c.k < 3 && t == 10*c.ms*c.k {
		c.prev = c.pos
		c.pos += c.pitch
		c.k++
		c.fade = c.quart
	}
	cur := c.loops[c.k-1]
	s := cur[c.pos]
	c.pos = (c.pos + 1) % len(cur)
	if c.fade > 0 {
		old := c.loops[c.k-2]
		b := float32(c.fade) / float32(c.quart+1)
		s = old[c.prev]*b + s*(1-b)
		c.prev = (c.prev + 1) % len(old)
		c.fade--
	}
	if t < c.quart {
		s += c.offset * float32(c.quart-t) / float32(c.quart)
	}
	if hold := plcHoldMs * c.ms; t >= hold {
		s *= float32(plcMuteMs*c.ms-t) / float32(plcMuteMs*c.ms-hold)
	}
	return s
}

// findPitch returns the lag, in samples, that best predicts the last 20 ms of
// history: a search at 8 kHz, then above that rate a fine one around its
// result.
func (c *Concealer) findPitch() int {
	r := c.ms / 8
	lo, hi := plcPitchMin*r, plcPitchMax*r
	best := c.bestLag(lo, hi, r)
	if r == 1 {
		return best
	}
	return c.bestLag(max(best-r+1, lo), min(best+r-1, hi), 1)
}

func (c *Concealer) bestLag(lo, hi, step int) int {
	h := c.hist
	n := len(h)
	l := plcCorrLen * c.ms / 8
	ref := h[n-l:]
	best, bestScore := lo, math.Inf(-1)
	for lag := lo; lag <= hi; lag += step {
		seg := h[n-l-lag : n-lag]
		var corr, energy float64
		for i := 0; i < l; i += step {
			corr += float64(ref[i]) * float64(seg[i])
			energy += float64(seg[i]) * float64(seg[i])
		}
		if energy == 0 {
			continue
		}
		if score := corr / math.Sqrt(energy); score > bestScore {
			best, bestScore = lag, score
		}
	}
	return best
}

func clampInt16(v float32) int16 {
	switch {
	case v >= math.MaxInt16:
		return math.MaxInt16
	case v <= math.MinInt16:
		return math.MinInt16
	}
	return int16(math.Round(float64(v)))
}

// concealingDecoder gives a decoder that has no loss concealment of its own a
// DecodeGap, by running a Concealer over what it decodes.
type concealingDecoder struct {
	Decoder
	plc   *Concealer
	frame int // samples in 20 ms
}

func withConcealment(dec Decoder, rate int) *concealingDecoder {
	return &concealingDecoder{Decoder: dec, plc: NewConcealer(rate), frame: rate / 50}
}

func (d *concealingDecoder) Decode(data []byte) ([]int16, error) {
	return d.DecodeInto(nil, data)
}

func (d *concealingDecoder) DecodeInto(dst []int16, data []byte) ([]int16, error) {
	out, err := DecodeInto(d.Decoder, dst, data)
	if err == nil {
		d.plc.Good(out)
	}
	return out, err
}

func (d *concealingDecoder) DecodeGap(dst []int16) ([]int16, error) {
	return d.plc.Conceal(dst, d.frame), nil
}

func (d *concealingDecoder) InDTX() bool { return false }

func (d *concealingDecoder) Reset() {
	d.Decoder.Reset()
	d.plc.Reset()
}
