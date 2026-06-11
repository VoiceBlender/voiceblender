package playback

import (
	"encoding/binary"
	"math"
	"sort"
	"strings"
)

// CadenceSegment defines a segment of a tone cadence pattern.
// On=true means tone is playing, On=false means silence.
type CadenceSegment struct {
	DurationMs int
	On         bool
}

// ToneSpec defines a telephone tone: frequencies, optional AM modulation, and cadence.
type ToneSpec struct {
	Frequencies  []float64        // base frequencies to mix (e.g. 440, 480)
	ModulationHz float64          // amplitude modulation frequency (0 = none)
	Cadence      []CadenceSegment // on/off pattern; nil or empty = continuous
}

// ToneReader generates infinite cadenced PCM from a ToneSpec.
// It implements io.Reader producing 16-bit signed little-endian mono PCM.
type ToneReader struct {
	spec       ToneSpec
	sampleRate int
	amplitude  float64 // peak amplitude (~0.7 * MaxInt16)

	// Phase accumulators (radians), one per frequency + modulation.
	phases   []float64
	modPhase float64

	// Cadence tracking.
	cadenceIdx    int   // current segment index
	cadenceSample int   // samples elapsed in current segment
	cadenceLens   []int // pre-computed segment lengths in samples

	// Fade ramp at cadence transitions to avoid clicks.
	fadeLen    int     // ramp length in samples (e.g. 2ms worth)
	fadeGain   float64 // current envelope gain (0..1)
	fadeTarget float64 // target gain (1 for on, 0 for off)
	fadeDelta  float64 // gain change per sample during ramp
}

// NewToneReader creates a ToneReader for the given spec at the given sample rate.
func NewToneReader(spec ToneSpec, sampleRate int) *ToneReader {
	// 2ms fade ramp to avoid clicks at cadence transitions.
	fadeLen := sampleRate * 2 / 1000
	if fadeLen < 1 {
		fadeLen = 1
	}

	// Determine initial gain based on whether the first cadence segment is on.
	initialGain := 1.0
	if len(spec.Cadence) > 0 && !spec.Cadence[0].On {
		initialGain = 0.0
	}

	tr := &ToneReader{
		spec:       spec,
		sampleRate: sampleRate,
		amplitude:  23000, // ~0.7 * 32767
		phases:     make([]float64, len(spec.Frequencies)),
		fadeLen:    fadeLen,
		fadeGain:   initialGain,
		fadeTarget: initialGain,
		fadeDelta:  1.0 / float64(fadeLen),
	}

	// Pre-compute cadence segment lengths in samples.
	if len(spec.Cadence) > 0 {
		tr.cadenceLens = make([]int, len(spec.Cadence))
		for i, seg := range spec.Cadence {
			tr.cadenceLens[i] = seg.DurationMs * sampleRate / 1000
		}
	}

	return tr
}

// Read fills p with 16-bit LE mono PCM. Never returns EOF.
func (tr *ToneReader) Read(p []byte) (int, error) {
	// Ensure we write whole samples (2 bytes each).
	nSamples := len(p) / 2
	if nSamples == 0 {
		return 0, nil
	}

	twoPi := 2.0 * math.Pi
	nFreqs := len(tr.spec.Frequencies)
	scale := 1.0
	if nFreqs > 1 {
		scale = 1.0 / float64(nFreqs)
	}

	for i := 0; i < nSamples; i++ {
		// Advance cadence and update fade target on segment transitions.
		if len(tr.cadenceLens) > 0 {
			tr.cadenceSample++
			if tr.cadenceSample >= tr.cadenceLens[tr.cadenceIdx] {
				tr.cadenceSample = 0
				tr.cadenceIdx = (tr.cadenceIdx + 1) % len(tr.cadenceLens)
				// Set new fade target on segment boundary.
				if tr.spec.Cadence[tr.cadenceIdx].On {
					tr.fadeTarget = 1.0
				} else {
					tr.fadeTarget = 0.0
				}
			}
		}

		// Ramp fadeGain toward fadeTarget.
		if tr.fadeGain < tr.fadeTarget {
			tr.fadeGain += tr.fadeDelta
			if tr.fadeGain > tr.fadeTarget {
				tr.fadeGain = tr.fadeTarget
			}
		} else if tr.fadeGain > tr.fadeTarget {
			tr.fadeGain -= tr.fadeDelta
			if tr.fadeGain < tr.fadeTarget {
				tr.fadeGain = tr.fadeTarget
			}
		}

		// Always generate the sine wave (keeps phase continuous).
		var val float64
		for fi := 0; fi < nFreqs; fi++ {
			val += math.Sin(tr.phases[fi])
			tr.phases[fi] += twoPi * tr.spec.Frequencies[fi] / float64(tr.sampleRate)
			if tr.phases[fi] >= twoPi {
				tr.phases[fi] -= twoPi
			}
		}
		val *= scale

		// Apply AM modulation if specified.
		if tr.spec.ModulationHz > 0 {
			mod := 0.5 + 0.5*math.Sin(tr.modPhase)
			val *= mod
			tr.modPhase += twoPi * tr.spec.ModulationHz / float64(tr.sampleRate)
			if tr.modPhase >= twoPi {
				tr.modPhase -= twoPi
			}
		}

		// Apply cadence envelope (fade ramp).
		sample := int16(val * tr.amplitude * tr.fadeGain)

		binary.LittleEndian.PutUint16(p[i*2:], uint16(sample))
	}

	return nSamples * 2, nil
}

// tone registry: "country_type" → ToneSpec
var toneRegistry = map[string]ToneSpec{
	// --- United States ---
	"us_ringback":   {Frequencies: []float64{440, 480}, Cadence: onOff(2000, 4000)},
	"us_busy":       {Frequencies: []float64{480, 620}, Cadence: onOff(500, 500)},
	"us_dial":       {Frequencies: []float64{350, 440}},
	"us_congestion": {Frequencies: []float64{480, 620}, Cadence: onOff(250, 250)},

	// --- United Kingdom ---
	"gb_ringback":   {Frequencies: []float64{400, 450}, Cadence: []CadenceSegment{{400, true}, {200, false}, {400, true}, {2000, false}}},
	"gb_busy":       {Frequencies: []float64{400}, Cadence: onOff(375, 375)},
	"gb_dial":       {Frequencies: []float64{350, 440}},
	"gb_congestion": {Frequencies: []float64{400}, Cadence: []CadenceSegment{{400, true}, {350, false}, {225, true}, {525, false}}},

	// --- Germany ---
	"de_ringback":   {Frequencies: []float64{425}, Cadence: onOff(1000, 4000)},
	"de_busy":       {Frequencies: []float64{425}, Cadence: onOff(480, 480)},
	"de_dial":       {Frequencies: []float64{425}},
	"de_congestion": {Frequencies: []float64{425}, Cadence: onOff(240, 240)},

	// --- France ---
	"fr_ringback":   {Frequencies: []float64{440}, Cadence: onOff(1500, 3500)},
	"fr_busy":       {Frequencies: []float64{440}, Cadence: onOff(500, 500)},
	"fr_dial":       {Frequencies: []float64{440}},
	"fr_congestion": {Frequencies: []float64{440}, Cadence: onOff(250, 250)},

	// --- Australia ---
	"au_ringback":   {Frequencies: []float64{400}, ModulationHz: 17, Cadence: []CadenceSegment{{400, true}, {200, false}, {400, true}, {2000, false}}},
	"au_busy":       {Frequencies: []float64{400}, Cadence: onOff(375, 375)},
	"au_dial":       {Frequencies: []float64{400, 450}},
	"au_congestion": {Frequencies: []float64{400}, Cadence: onOff(375, 375)},

	// --- Japan ---
	"jp_ringback":   {Frequencies: []float64{400}, ModulationHz: 16, Cadence: onOff(1000, 2000)},
	"jp_busy":       {Frequencies: []float64{400}, Cadence: onOff(500, 500)},
	"jp_dial":       {Frequencies: []float64{400}},
	"jp_congestion": {Frequencies: []float64{400}, Cadence: onOff(250, 250)},

	// --- Italy ---
	"it_ringback":   {Frequencies: []float64{425}, Cadence: onOff(1000, 4000)},
	"it_busy":       {Frequencies: []float64{425}, Cadence: onOff(500, 500)},
	"it_dial":       {Frequencies: []float64{425}},
	"it_congestion": {Frequencies: []float64{425}, Cadence: onOff(200, 200)},

	// --- India ---
	"in_ringback":   {Frequencies: []float64{400}, ModulationHz: 25, Cadence: []CadenceSegment{{400, true}, {200, false}, {400, true}, {2600, false}}},
	"in_busy":       {Frequencies: []float64{400}, Cadence: onOff(750, 750)},
	"in_dial":       {Frequencies: []float64{400}},
	"in_congestion": {Frequencies: []float64{400}, Cadence: onOff(250, 250)},

	// --- Brazil ---
	"br_ringback":   {Frequencies: []float64{425}, Cadence: onOff(1000, 4000)},
	"br_busy":       {Frequencies: []float64{425}, Cadence: onOff(250, 250)},
	"br_dial":       {Frequencies: []float64{425}},
	"br_congestion": {Frequencies: []float64{425}, Cadence: onOff(250, 250)},

	// --- Poland ---
	"pl_ringback": {Frequencies: []float64{425}, Cadence: onOff(1000, 4000)},

	// --- Russia ---
	"ru_ringback":   {Frequencies: []float64{425}, Cadence: onOff(800, 3200)},
	"ru_busy":       {Frequencies: []float64{425}, Cadence: onOff(350, 350)},
	"ru_dial":       {Frequencies: []float64{425}},
	"ru_congestion": {Frequencies: []float64{425}, Cadence: onOff(175, 175)},
}

// onOff is a helper to create a simple on/off cadence pattern.
func onOff(onMs, offMs int) []CadenceSegment {
	return []CadenceSegment{{onMs, true}, {offMs, false}}
}

// countryAliases maps common alternative country codes to their canonical form.
var countryAliases = map[string]string{
	"uk": "gb",
}

// LookupTone finds a tone by name. Accepts "country_type" (e.g. "us_ringback")
// or bare "type" (e.g. "ringback") which defaults to US.
// Also accepts common aliases (e.g. "uk_ringback" → "gb_ringback").
func LookupTone(name string) (ToneSpec, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	if spec, ok := toneRegistry[name]; ok {
		return spec, true
	}
	// Try country alias (e.g. uk → gb).
	if idx := strings.Index(name, "_"); idx > 0 {
		if canonical, ok := countryAliases[name[:idx]]; ok {
			if spec, ok := toneRegistry[canonical+name[idx:]]; ok {
				return spec, true
			}
		}
	}
	// Bare name → default to US.
	if !strings.Contains(name, "_") {
		if spec, ok := toneRegistry["us_"+name]; ok {
			return spec, true
		}
	}
	return ToneSpec{}, false
}

// ToneNames returns a sorted list of all registered tone names.
func ToneNames() []string {
	names := make([]string, 0, len(toneRegistry))
	for k := range toneRegistry {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
