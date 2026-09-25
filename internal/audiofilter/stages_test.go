package audiofilter

import (
	"math"
	"testing"
)

// TestBandpassAttenuatesOutOfBand checks the filter actually filters, and that
// its state carries across frames rather than restarting each block.
func TestBandpassAttenuatesOutOfBand(t *testing.T) {
	const rate = 16000
	for _, c := range []struct {
		name    string
		hz      float64
		wantMax float64 // upper bound on retained fraction
		wantMin float64
	}{
		{"in-band 1 kHz", 1000, 1.10, 0.85},
		{"below band 60 Hz", 60, 0.10, 0},
		{"above band 7 kHz", 7000, 0.20, 0},
	} {
		st, err := newBandpass(rate, 300, 3400)
		if err != nil {
			t.Fatal(err)
		}
		in := tone(rate, rate, c.hz, 8000)
		buf := append([]int16(nil), in...)
		for off := 0; off+320 <= len(buf); off += 320 { // 20 ms frames
			st.Process(buf[off : off+320])
		}
		// Skip the settling transient.
		ratio := rms(buf[rate/2:]) / rms(in[rate/2:])
		t.Logf("%-18s retained %.3f of input RMS", c.name, ratio)
		if ratio > c.wantMax || ratio < c.wantMin {
			t.Errorf("%s: retained %.3f, want between %.2f and %.2f", c.name, ratio, c.wantMin, c.wantMax)
		}
	}
}

func TestBandpassRejectsBadBand(t *testing.T) {
	for _, c := range []struct{ low, high float64 }{{0, 3400}, {-1, 3400}, {3400, 300}, {300, 300}} {
		if _, err := newBandpass(16000, c.low, c.high); err == nil {
			t.Errorf("band %g-%g should be rejected", c.low, c.high)
		}
	}
	// Above Nyquist is clamped, not rejected.
	if _, err := newBandpass(8000, 300, 9000); err != nil {
		t.Errorf("high_hz above Nyquist should clamp, got %v", err)
	}
	t.Log("invalid bands rejected; above-Nyquist clamped")
}

// TestGainMatchesPlaybackCurve pins the filter to the same ~3 dB per step curve
// the playback/TTS volume control uses, so the two cannot drift apart.
func TestGainMatchesPlaybackCurve(t *testing.T) {
	for _, v := range []float64{-8, -4, -1, 0, 1, 4, 8} {
		s, err := newGain(v)
		if err != nil {
			t.Fatal(err)
		}
		got := s.(*gainStage).gain
		want := math.Pow(10, v*0.15)
		if math.Abs(got-want) > 1e-12 {
			t.Errorf("volume %g: gain %g, want %g", v, got, want)
			continue
		}
		t.Logf("volume %+.0f -> gain %.4f (%+.1f dB)", v, got, 20*math.Log10(got))
	}
	if db := 20 * math.Log10(math.Pow(10, 0.15)); math.Abs(db-3) > 0.01 {
		t.Errorf("one step is %.2f dB, want ~3 dB", db)
	}
}

func TestGainAppliesAndClamps(t *testing.T) {
	s, err := newGain(0)
	if err != nil {
		t.Fatal(err)
	}
	in := []int16{100, -100, 32767, -32768}
	frame := append([]int16(nil), in...)
	s.Process(frame)
	for i := range in {
		if frame[i] != in[i] {
			t.Errorf("volume 0 must be a no-op: %v -> %v", in, frame)
			break
		}
	}

	s, _ = newGain(8)
	frame = []int16{32767, -32768, 1000}
	s.Process(frame)
	if frame[0] != 32767 || frame[1] != -32768 {
		t.Errorf("boost must clamp at full scale, got %v", frame[:2])
	}
	if frame[2] <= 1000 {
		t.Errorf("boost should raise a mid-scale sample, got %d", frame[2])
	}
	t.Logf("volume +8 on {32767,-32768,1000} -> %v", frame)
}

func TestGainRejectsOutOfRange(t *testing.T) {
	for _, v := range []float64{-9, 9, 100, 1.5} {
		if _, err := newGain(v); err == nil {
			t.Errorf("volume %g should be rejected", v)
		}
	}
	t.Logf("volume range enforced: %d to %d, whole steps", MinVolume, MaxVolume)
}
