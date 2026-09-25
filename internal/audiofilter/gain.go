package audiofilter

import (
	"fmt"
	"math"
)

// Volume range and curve match the playback/TTS control (applyVolume in
// internal/playback): steps of ~3 dB over -8..8, so operators have one
// vocabulary for level regardless of which audio they are adjusting.
const (
	MinVolume = -8
	MaxVolume = 8
)

func init() {
	Register("gain", Descriptor{
		ValidateParams: func(p Params) error {
			_, err := newGain(p.Get("volume", 0))
			return err
		},
		New: func(_ int, p Params) (Stage, error) {
			return newGain(p.Get("volume", 0))
		},
	})
}

type gainStage struct{ gain float64 }

func newGain(volume float64) (Stage, error) {
	if volume != math.Trunc(volume) {
		return nil, fmt.Errorf("volume must be a whole number of steps, got %g", volume)
	}
	if volume < MinVolume || volume > MaxVolume {
		return nil, fmt.Errorf("volume %g out of range (%d to %d)", volume, MinVolume, MaxVolume)
	}
	return &gainStage{gain: math.Pow(10, volume*0.15)}, nil
}

func (g *gainStage) Close() error { return nil }

func (g *gainStage) Process(frame []int16) {
	if g.gain == 1 {
		return
	}
	for i, s := range frame {
		v := float64(s) * g.gain
		if v > 32767 {
			v = 32767
		} else if v < -32768 {
			v = -32768
		}
		frame[i] = int16(v)
	}
}
