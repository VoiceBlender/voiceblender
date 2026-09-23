package denoise

import (
	"math"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/audiofilter"
)

// legRates are the rates a leg actually arrives at: G.711/G.729 at 8 kHz,
// G.722 and the default mixer at 16 kHz, Opus at 48 kHz.
var legRates = []int{8000, 16000, 48000}

// TestChainAddsNoResamplingForDenoise is the load-bearing one: denoise declares
// no required rate, so a chain containing it runs at the rate the leg and room
// already agreed on. Pinning the filter to 48 kHz would resample every
// narrowband leg up and back down for the filter's sake, which costs latency
// and CPU and recovers nothing -- there is no content above the leg's own
// Nyquist to recover.
func TestChainAddsNoResamplingForDenoise(t *testing.T) {
	install(t)
	specs := audiofilter.Resolve([]audiofilter.Spec{{Type: "denoise"}})
	if len(specs) != 1 {
		t.Fatal("denoise should be available here")
	}
	for _, rate := range legRates {
		if got := audiofilter.ResolveWorkRate(specs, rate); got != rate {
			t.Errorf("room at %d Hz: chain resolved to %d Hz, want %d", rate, got, rate)
			continue
		}
		t.Logf("room at %5d Hz -> chain works at %5d Hz (no conversion added)", rate, rate)
	}

	// And with an effect alongside it, which is also rate-agnostic.
	mixed := audiofilter.Resolve([]audiofilter.Spec{{Type: "denoise"}, {Type: "bandpass"}})
	if got := audiofilter.ResolveWorkRate(mixed, 8000); got != 8000 {
		t.Errorf("denoise+bandpass at 8 kHz resolved to %d Hz, want 8000", got)
	}
}

// TestNativeRates checks the stage really runs at each rate, with the 10 ms
// frame that rate implies, and suppresses noise there.
func TestNativeRates(t *testing.T) {
	install(t)
	for _, rate := range legRates {
		st, err := current().acquire(rate)
		if err != nil {
			t.Errorf("acquire at %d Hz: %v", rate, err)
			continue
		}
		if want := rate / 100; st.frame != want {
			t.Errorf("%d Hz: frame is %d samples, want %d (10 ms)", rate, st.frame, want)
			st.release()
			continue
		}

		// A quarter of the band, so the test signal is inside every rate's
		// passband and the numbers are comparable across them.
		in := bandLimitedAt(st.frame*200, rate, 7, 2000, float64(rate)/4)
		out := append([]int16(nil), in...)
		for off := 0; off+st.frame <= len(out); off += st.frame {
			if err := st.process(out[off : off+st.frame]); err != nil {
				t.Fatalf("%d Hz: process: %v", rate, err)
			}
		}
		from := st.frame * 100 // skip the convergence ramp
		before, after := rms(in[from:]), rms(out[from:])
		db := 20 * math.Log10(after/before)
		t.Logf("%5d Hz: frame %3d samples, steady-state suppression %+.1f dB", rate, st.frame, db)
		if db > -10 {
			t.Errorf("%d Hz: expected at least 10 dB suppression, got %+.1f dB", rate, db)
		}
		st.release()
	}
}

// TestStageReportsItsOwnFrame: the chain sizes its block from the stage, not
// from the descriptor, because the frame follows the rate. A chain built at a
// rate whose frame it guessed wrong would feed the denoiser short blocks.
func TestStageReportsItsOwnFrame(t *testing.T) {
	install(t)
	for _, rate := range legRates {
		st, err := newStage(rate, nil)
		if err != nil {
			t.Fatalf("newStage at %d Hz: %v", rate, err)
		}
		fs, ok := st.(audiofilter.FrameSizer)
		if !ok {
			t.Fatal("the denoise stage must report its frame length to the chain")
		}
		if got, want := fs.FrameSamples(), rate/100; got != want {
			t.Errorf("%d Hz: stage reports %d samples, want %d", rate, got, want)
		}
		st.Close()
	}
	t.Log("stage frame length follows the chain's working rate")
}

// TestRejectsRateBelowModelMinimum: below 8 kHz the model would be
// extrapolating past anything it was trained on, so the chain must fail to
// build rather than produce audio nobody can vouch for.
func TestRejectsRateBelowModelMinimum(t *testing.T) {
	install(t)
	if _, err := newStage(MinRate-1000, nil); err == nil {
		t.Errorf("a rate below %d Hz should be rejected", MinRate)
	} else {
		t.Logf("rejected: %v", err)
	}
	if _, err := newStage(MinRate, nil); err != nil {
		t.Errorf("%d Hz is the documented minimum and must work: %v", MinRate, err)
	}
}

// TestPoolIsKeyedByRate: a Denoiser's tables are rate-specific, so a state
// released by an 8 kHz leg must never be handed to a 16 kHz one.
func TestPoolIsKeyedByRate(t *testing.T) {
	install(t)
	k := current()

	a, err := k.acquire(8000)
	if err != nil {
		t.Fatal(err)
	}
	a.release()

	b, err := k.acquire(16000)
	if err != nil {
		t.Fatal(err)
	}
	defer b.release()
	if b.frame != 160 {
		t.Fatalf("16 kHz state has a %d-sample frame; it was recycled from another rate", b.frame)
	}
	// The 8 kHz state is still there for an 8 kHz leg.
	c, err := k.acquire(8000)
	if err != nil {
		t.Fatal(err)
	}
	defer c.release()
	if c.frame != 80 {
		t.Fatalf("8 kHz state has a %d-sample frame", c.frame)
	}
	t.Log("states are pooled per rate; a released state is only reused at its own rate")
}

// bandLimitedAt is bandLimited at an arbitrary rate.
func bandLimitedAt(n, rate int, seed int64, amp, fc float64) []int16 {
	src := noise(n, seed, amp)
	out := make([]int16, n)
	a := math.Exp(-2 * math.Pi * fc / float64(rate))
	var y float64
	for i, v := range src {
		y = (1-a)*float64(v) + a*y
		out[i] = int16(y * 3)
	}
	return out
}
