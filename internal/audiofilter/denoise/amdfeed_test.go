package denoise

import (
	"bytes"
	"math"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/audiofilter"
	"github.com/VoiceBlender/voiceblender/internal/speaking"
)

// TestWriterFeedsDetectorsFilteredAudio covers the push-mode chain that
// answering-machine detection uses. AMD starts during ringing, before the leg
// joins a room, so it cannot sit behind the room's Reader — it drives a chain
// of its own from the write side. It must still get denoised audio: an energy
// FSM and a beep detector are exactly what a raised noise floor misleads.
func TestWriterFeedsDetectorsFilteredAudio(t *testing.T) {
	install(t, 0)
	const legRate, amdRate = 8000, 16000

	// Noise-only, as a ringing trunk with a noisy far end would deliver.
	noisy := bandLimited(legRate*6, 9, float64(speaking.Threshold)*3, 3000)

	var sink bytes.Buffer
	w, err := audiofilter.NewWriter(&sink, legRate, amdRate,
		[]audiofilter.Spec{{Type: "denoise"}})
	if err != nil {
		t.Fatal(err)
	}
	for off := 0; off+160 <= len(noisy); off += 160 { // 20 ms frames at 8 kHz
		if _, err := w.Write(toBytes(noisy[off : off+160])); err != nil {
			t.Fatal(err)
		}
	}
	out := toSamples(sink.Bytes())
	if len(out) == 0 {
		t.Fatal("the AMD feed produced no audio")
	}

	// Resampled 8 kHz -> 16 kHz, so roughly double the samples.
	if ratio := float64(len(out)) / float64(len(noisy)); ratio < 1.8 || ratio > 2.2 {
		t.Errorf("expected ~2x samples for 8k->16k, got %.2fx", ratio)
	}

	before, after := rms(noisy[legRate*2:]), rms(out[amdRate*2:])
	t.Logf("AMD feed: input RMS %.0f -> %.0f (%+.1f dB), %d -> %d samples",
		before, after, 20*math.Log10(after/before), len(noisy), len(out))
	if after >= before/2 {
		t.Errorf("the AMD feed is not denoising: %.0f -> %.0f", before, after)
	}

	// Closing must hand the kernel state back, or every AMD attempt leaks one.
	live, _ := Stats()
	if live == 0 {
		t.Fatal("expected a live kernel state while the feed is open")
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if live, _ = Stats(); live != 0 {
		t.Errorf("closing the AMD feed leaked %d kernel state(s)", live)
	}
	t.Log("feed closed; kernel state returned to the pool")
}

// TestWriterWithoutFiltersIsPlainResampling pins the zero-cost path: a leg with
// no corrective filters must get exactly the resampling writer AMD used before.
func TestWriterWithoutFiltersIsPlainResampling(t *testing.T) {
	var sink bytes.Buffer
	w, err := audiofilter.NewWriter(&sink, 8000, 16000, nil)
	if err != nil {
		t.Fatal(err)
	}
	in := bandLimited(8000, 1, 4000, 3000)
	for off := 0; off+160 <= len(in); off += 160 {
		w.Write(toBytes(in[off : off+160]))
	}
	out := toSamples(sink.Bytes())
	if ratio := float64(len(out)) / float64(len(in)); ratio < 1.8 || ratio > 2.2 {
		t.Errorf("expected ~2x samples, got %.2fx", ratio)
	}
	if live, _ := Stats(); live != 0 {
		t.Errorf("an unfiltered feed should hold no kernel state, got %d", live)
	}
	t.Logf("no filters: %d -> %d samples, no kernel state held", len(in), len(out))
}
