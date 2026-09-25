package denoise

import (
	"io"
	"math"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/audiofilter"
	"github.com/VoiceBlender/voiceblender/internal/speaking"
)

// TestDenoiseStopsVADFiringOnNoise is the case that motivates running voice
// activity detection after the filter chain rather than before it. The
// detector is RMS with hysteresis, so a raised noise floor alone trips it —
// exactly what denoise removes. Scoring the audio upstream of the chain means
// the leg reports speech while nobody is talking.
func TestDenoiseStopsVADFiringOnNoise(t *testing.T) {
	install(t)
	const rate = 16000

	// Background noise only — no speech at any point. Band-limited, as every
	// codec on this path delivers, and comfortably over the VAD threshold.
	noisy := bandLimited(rate*6, 3, float64(speaking.Threshold)*3, 3400)

	raw := countSpeakingFrames(t, noisy, rate)
	filtered := countSpeakingFrames(t, denoiseThrough(t, noisy, rate), rate)

	t.Logf("noise-only input, VAD threshold %d", speaking.Threshold)
	t.Logf("  before denoise: %d of %d frames over threshold", raw.over, raw.total)
	t.Logf("  after  denoise: %d of %d frames over threshold", filtered.over, filtered.total)

	if raw.over == 0 {
		t.Fatal("test signal does not trip the VAD at all; it cannot show the difference")
	}
	if filtered.over >= raw.over {
		t.Errorf("denoise did not reduce false speech frames: %d -> %d", raw.over, filtered.over)
	}
	// The noise floor should end up well under the threshold, not merely lower.
	if filtered.over > raw.total/20 {
		t.Errorf("%d of %d frames still read as speech after denoise", filtered.over, filtered.total)
	}
}

type vadCount struct{ over, total int }

// countSpeakingFrames scores 20 ms frames the way speaking.Detector does.
func countSpeakingFrames(t *testing.T, pcm []int16, rate int) vadCount {
	t.Helper()
	frame := rate / 50
	var c vadCount
	// Skip the first second: denoise's noise estimate is still converging.
	for off := rate; off+frame <= len(pcm); off += frame {
		c.total++
		if speaking.ComputeRMS(pcm[off:off+frame]) >= speaking.Threshold {
			c.over++
		}
	}
	return c
}

// denoiseThrough runs audio through a denoise chain, as the room does.
func denoiseThrough(t *testing.T, in []int16, rate int) []int16 {
	t.Helper()
	src := &sliceReader{data: toBytes(in), n: rate / 50 * 2}
	r, err := audiofilter.Build(src, rate, rate, []audiofilter.Spec{{Type: "denoise"}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return toSamples(out)
}

// TestObserverSeesFilteredAudio pins the wiring: an observer on the chain must
// receive post-filter audio, or the detector is back to scoring the noise.
func TestObserverSeesFilteredAudio(t *testing.T) {
	install(t)
	const rate = 16000
	noisy := bandLimited(rate*4, 5, float64(speaking.Threshold)*3, 3400)

	src := &sliceReader{data: toBytes(noisy), n: rate / 50 * 2}
	r, err := audiofilter.Build(src, rate, rate, []audiofilter.Spec{{Type: "denoise"}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	var seen collector
	r.SetObserver("test", &seen)
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}

	observed := toSamples(seen.b)
	produced := toSamples(out)
	if len(observed) != len(produced) {
		t.Fatalf("observer saw %d samples, the reader produced %d", len(observed), len(produced))
	}
	for i := range produced {
		if observed[i] != produced[i] {
			t.Fatalf("observer diverged from the output at sample %d", i)
		}
	}
	inRMS, outRMS := rms(noisy[rate:]), rms(observed[rate:])
	t.Logf("observer RMS %.0f vs raw input %.0f (%+.1f dB): it is seeing filtered audio",
		outRMS, inRMS, 20*math.Log10(outRMS/inRMS))
	if outRMS >= inRMS {
		t.Error("observer is not seeing filtered audio")
	}
}

type collector struct{ b []byte }

func (c *collector) Write(p []byte) (int, error) { c.b = append(c.b, p...); return len(p), nil }
