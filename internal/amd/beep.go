package amd

import "math"

// beepDetector uses the Goertzel algorithm to detect a sustained pure tone
// (beep) in a stream of 16 kHz PCM frames. Voicemail beeps are typically a
// single frequency between 800–1200 Hz lasting 100–500 ms.
type beepDetector struct {
	// Target frequency range.
	minFreq float64
	maxFreq float64

	// Detection thresholds.
	energyRatio float64 // min ratio of target bin energy to total energy
	minFrames   int     // consecutive frames with tone to confirm beep

	// State.
	consecutiveFrames int
	detected          bool
}

// newBeepDetector creates a detector scanning for tones in [minFreq, maxFreq].
// energyRatio is the minimum fraction of frame energy that must be in the
// target band (0.3 = 30%). minFrames is how many consecutive 20 ms frames
// must contain the tone before declaring a beep.
func newBeepDetector(minFreq, maxFreq, energyRatio float64, minFrames int) *beepDetector {
	return &beepDetector{
		minFreq:     minFreq,
		maxFreq:     maxFreq,
		energyRatio: energyRatio,
		minFrames:   minFrames,
	}
}

// feed processes one frame of PCM samples and returns true if a beep has been
// confirmed (enough consecutive tonal frames).
func (d *beepDetector) feed(samples []int16) bool {
	if d.detected {
		return true
	}

	if isTonal(samples, sampleRate, d.minFreq, d.maxFreq, d.energyRatio) {
		d.consecutiveFrames++
		if d.consecutiveFrames >= d.minFrames {
			d.detected = true
			return true
		}
	} else {
		d.consecutiveFrames = 0
	}
	return false
}

// isTonal checks whether the dominant energy in samples falls within
// [minFreq, maxFreq] using the Goertzel algorithm. It evaluates several
// candidate frequencies across the band and compares the strongest to the
// total frame energy.
func isTonal(samples []int16, sr int, minFreq, maxFreq, ratioThreshold float64) bool {
	n := len(samples)
	if n == 0 {
		return false
	}

	// Total energy of the frame.
	var totalEnergy float64
	for _, s := range samples {
		v := float64(s)
		totalEnergy += v * v
	}
	if totalEnergy == 0 {
		return false
	}

	// Scan candidate frequencies across the target band in 50 Hz steps.
	step := 50.0
	var maxBinEnergy float64
	for freq := minFreq; freq <= maxFreq; freq += step {
		e := goertzelEnergy(samples, sr, freq)
		if e > maxBinEnergy {
			maxBinEnergy = e
		}
	}

	// Normalize: Goertzel energy is proportional to N^2 × amplitude^2 for a
	// pure tone, while totalEnergy is N × amplitude^2. Divide by N to make
	// the ratio comparable.
	normalizedBin := maxBinEnergy / float64(n)
	return normalizedBin/totalEnergy >= ratioThreshold
}

// goertzelEnergy computes the energy at a single target frequency using the
// Goertzel algorithm. This is O(N) and much cheaper than a full FFT when only
// one (or a few) frequencies are of interest.
func goertzelEnergy(samples []int16, sampleRateHz int, targetFreq float64) float64 {
	n := len(samples)
	k := int(math.Round(float64(n) * targetFreq / float64(sampleRateHz)))
	w := 2.0 * math.Pi * float64(k) / float64(n)
	coeff := 2.0 * math.Cos(w)

	var s0, s1, s2 float64
	for _, sample := range samples {
		s0 = float64(sample) + coeff*s1 - s2
		s2 = s1
		s1 = s0
	}
	// Energy = |X(k)|^2
	return s1*s1 + s2*s2 - coeff*s1*s2
}
