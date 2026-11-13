package codec

import (
	"math"
	"testing"

	"github.com/thesyncim/gopus"
)

// encodeSpeechLike encodes 2 s of a tone rich in high harmonics and returns
// the average bitrate and the widest bandwidth any packet used.
func encodeSpeechLike(t *testing.T, e *OpusEncoder) (kbps float64, widest gopus.Bandwidth) {
	t.Helper()
	const frames = 100
	total := 0
	frame := make([]int16, 960)
	for f := 0; f < frames; f++ {
		for i := range frame {
			x := float64(f*len(frame)+i) / 48000
			frame[i] = int16(4000*math.Sin(2*math.Pi*300*x) + 3000*math.Sin(2*math.Pi*3100*x) +
				2000*math.Sin(2*math.Pi*7000*x) + 2000*math.Sin(2*math.Pi*14000*x))
		}
		out, err := e.Encode(frame)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		total += len(out)
		if bw := gopus.ParseTOC(out[0]).Bandwidth; bw > widest {
			widest = bw
		}
	}
	return float64(total*8) / (frames * 0.02) / 1000, widest
}

func TestOpusEncoder_DefaultBitrate(t *testing.T) {
	e, err := NewOpusEncoder()
	if err != nil {
		t.Fatal(err)
	}
	if got := e.Bitrate(); got != DefaultOpusBitrate {
		t.Fatalf("Bitrate() = %d, want %d", got, DefaultOpusBitrate)
	}
	kbps, widest := encodeSpeechLike(t, e)
	if kbps < 16 || kbps > 32 {
		t.Errorf("default encoder sends %.1f kbps, want about 24", kbps)
	}
	if widest != gopus.BandwidthFullband {
		t.Errorf("default encoder's widest bandwidth = %v, want fullband", widest)
	}
}

func TestOpusEncoder_Apply(t *testing.T) {
	for _, tc := range []struct {
		name       string
		p          OpusParams
		wantRate   int
		maxKbps    float64
		maxBW      gopus.Bandwidth
		wantWidest bool
	}{
		{"zero bitrate is the default", OpusParams{}, DefaultOpusBitrate, 32, gopus.BandwidthFullband, true},
		{"low bitrate", OpusParams{Bitrate: 10000}, 10000, 14, gopus.BandwidthFullband, false},
		{"narrowband receiver", OpusParams{Bitrate: 24000, MaxPlaybackRate: 8000}, 24000, 32, gopus.BandwidthNarrowband, true},
		{"wideband receiver", OpusParams{Bitrate: 24000, MaxPlaybackRate: 16000}, 24000, 32, gopus.BandwidthWideband, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, err := NewOpusEncoder()
			if err != nil {
				t.Fatal(err)
			}
			if err := e.Apply(tc.p); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if got := e.Bitrate(); got != tc.wantRate {
				t.Errorf("Bitrate() = %d, want %d", got, tc.wantRate)
			}
			kbps, widest := encodeSpeechLike(t, e)
			if kbps > tc.maxKbps {
				t.Errorf("sends %.1f kbps, want at most %.0f", kbps, tc.maxKbps)
			}
			if widest > tc.maxBW || (tc.wantWidest && widest != tc.maxBW) {
				t.Errorf("widest bandwidth = %v, want %v", widest, tc.maxBW)
			}
		})
	}
}

func TestOpusBandwidthFor(t *testing.T) {
	for hz, want := range map[int]gopus.Bandwidth{
		0:     gopus.BandwidthFullband,
		8000:  gopus.BandwidthNarrowband,
		11025: gopus.BandwidthMediumband,
		12000: gopus.BandwidthMediumband,
		16000: gopus.BandwidthWideband,
		22050: gopus.BandwidthSuperwideband,
		24000: gopus.BandwidthSuperwideband,
		44100: gopus.BandwidthFullband,
		48000: gopus.BandwidthFullband,
	} {
		if got := opusBandwidthFor(hz); got != want {
			t.Errorf("opusBandwidthFor(%d) = %v, want %v", hz, got, want)
		}
	}
}
