package comfortnoise

import (
	"math"
	"testing"
)

func TestEnabledGeneratorProducesNonZero(t *testing.T) {
	g := NewGenerator()
	samples := g.Generate(320)
	allZero := true
	for _, s := range samples {
		if s != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		t.Fatal("expected non-zero samples from enabled generator")
	}
}

func TestDisabledGeneratorProducesZeros(t *testing.T) {
	g := NewGenerator()
	g.SetEnabled(false)
	samples := g.Generate(320)
	for i, s := range samples {
		if s != 0 {
			t.Fatalf("sample[%d] = %d, want 0 (disabled)", i, s)
		}
	}
}

func TestAmplitudeClamping(t *testing.T) {
	// Below minimum
	g := NewGeneratorWithAmplitude(0)
	g.mu.Lock()
	if g.amplitude != minAmplitude {
		t.Errorf("amplitude = %d, want %d (min clamp)", g.amplitude, minAmplitude)
	}
	g.mu.Unlock()

	// Above maximum
	g = NewGeneratorWithAmplitude(200)
	g.mu.Lock()
	if g.amplitude != maxAmplitude {
		t.Errorf("amplitude = %d, want %d (max clamp)", g.amplitude, maxAmplitude)
	}
	g.mu.Unlock()

	// SetAmplitude also clamps
	g.SetAmplitude(-5)
	g.mu.Lock()
	if g.amplitude != minAmplitude {
		t.Errorf("SetAmplitude(-5): amplitude = %d, want %d", g.amplitude, minAmplitude)
	}
	g.mu.Unlock()

	g.SetAmplitude(999)
	g.mu.Lock()
	if g.amplitude != maxAmplitude {
		t.Errorf("SetAmplitude(999): amplitude = %d, want %d", g.amplitude, maxAmplitude)
	}
	g.mu.Unlock()
}

func TestAddToMixesIntoExisting(t *testing.T) {
	g := NewGenerator()
	// Start with all 1000s
	samples := make([]int16, 320)
	for i := range samples {
		samples[i] = 1000
	}
	g.AddTo(samples)
	allSame := true
	for _, s := range samples {
		if s != 1000 {
			allSame = false
			break
		}
	}
	if allSame {
		t.Fatal("AddTo should have modified at least some samples")
	}
}

func TestOutputWithinAmplitudeRange(t *testing.T) {
	amp := int16(50)
	g := NewGeneratorWithAmplitude(amp)
	samples := g.Generate(10000)
	// Verify RMS is reasonable: should be well below the amplitude value.
	var sumSq float64
	for _, s := range samples {
		sumSq += float64(s) * float64(s)
	}
	rms := math.Sqrt(sumSq / float64(len(samples)))
	// RMS should be meaningfully above zero but well below amplitude.
	if rms < 1 {
		t.Fatalf("RMS = %.2f, too low (generator may be silent)", rms)
	}
	if rms > float64(amp) {
		t.Fatalf("RMS = %.2f, exceeds amplitude %d", rms, amp)
	}
}
