package comfortnoise

import (
	"math"
	"math/rand"
	"sync"
)

const (
	defaultAmplitude int16 = 4 // ≈ -75 dBFS
	minAmplitude     int16 = 1
	maxAmplitude     int16 = 100
	filterAlpha            = 0.1
	warmupSamples          = 320
)

// Generator produces low-level comfort noise (filtered white noise)
// to inject into silent mixer frames so the connection doesn't feel dead.
type Generator struct {
	mu           sync.Mutex
	amplitude    int16
	enabled      bool
	rng          *rand.Rand
	filterState  float64
	filterState2 float64
}

// NewGenerator returns a Generator with default amplitude (4 ≈ -75 dBFS).
func NewGenerator() *Generator {
	return NewGeneratorWithAmplitude(defaultAmplitude)
}

// NewGeneratorWithAmplitude returns a Generator with a custom amplitude
// clamped to [1, 100]. The filter is pre-warmed with 320 samples.
func NewGeneratorWithAmplitude(amplitude int16) *Generator {
	if amplitude < minAmplitude {
		amplitude = minAmplitude
	}
	if amplitude > maxAmplitude {
		amplitude = maxAmplitude
	}
	g := &Generator{
		amplitude: amplitude,
		enabled:   true,
		rng:       rand.New(rand.NewSource(rand.Int63())),
	}
	// Pre-warm the IIR filter so output is stable from the first real frame.
	for i := 0; i < warmupSamples; i++ {
		g.nextSample()
	}
	return g
}

// nextSample generates one comfort noise sample through a two-stage
// IIR low-pass filter. Caller must hold mu.
//
// White noise in [-1,1] is filtered through two cascaded first-order IIR
// stages (alpha=0.1 each). The two stages attenuate the RMS by ~1/33,
// so filterCompensation rescales the output so peaks land near ±amplitude.
func (g *Generator) nextSample() int16 {
	const filterCompensation = 3.5 // compensates two-stage IIR attenuation (RMS ≈ 0.094 for unit input)
	raw := g.rng.Float64()*2 - 1
	g.filterState = g.filterState*(1-filterAlpha) + raw*filterAlpha
	g.filterState2 = g.filterState2*(1-filterAlpha) + g.filterState*filterAlpha
	return int16(g.filterState2 * float64(g.amplitude) * filterCompensation)
}

// Generate returns a new buffer of numSamples filled with comfort noise.
func (g *Generator) Generate(numSamples int) []int16 {
	out := make([]int16, numSamples)
	g.GenerateInto(out)
	return out
}

// GenerateInto overwrites the given buffer with comfort noise samples.
func (g *Generator) GenerateInto(samples []int16) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.enabled {
		for i := range samples {
			samples[i] = 0
		}
		return
	}
	for i := range samples {
		samples[i] = g.nextSample()
	}
}

// AddTo mixes comfort noise into the existing samples with int16 clamping.
func (g *Generator) AddTo(samples []int16) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.enabled {
		return
	}
	for i := range samples {
		s := int32(samples[i]) + int32(g.nextSample())
		if s > math.MaxInt16 {
			s = math.MaxInt16
		}
		if s < math.MinInt16 {
			s = math.MinInt16
		}
		samples[i] = int16(s)
	}
}

// SetAmplitude updates the noise amplitude, clamped to [1, 100].
func (g *Generator) SetAmplitude(amplitude int16) {
	if amplitude < minAmplitude {
		amplitude = minAmplitude
	}
	if amplitude > maxAmplitude {
		amplitude = maxAmplitude
	}
	g.mu.Lock()
	g.amplitude = amplitude
	g.mu.Unlock()
}

// SetEnabled toggles comfort noise generation on or off.
func (g *Generator) SetEnabled(enabled bool) {
	g.mu.Lock()
	g.enabled = enabled
	g.mu.Unlock()
}

// IsEnabled returns whether comfort noise generation is enabled.
func (g *Generator) IsEnabled() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.enabled
}
