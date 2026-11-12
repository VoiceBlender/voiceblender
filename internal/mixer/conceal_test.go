package mixer

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"math"
	"slices"
	"testing"
)

// voicedFrame is 20 ms of a speech-like signal: four harmonics of 110 Hz under
// a slow swell.
func voicedFrame(rate, f int) []int16 {
	out := make([]int16, rate/50)
	for i := range out {
		t := float64(f*len(out)+i) / float64(rate)
		var v float64
		for h := 1; h <= 4; h++ {
			v += math.Sin(2*math.Pi*110*float64(h)*t+float64(h)) / float64(h*h)
		}
		out[i] = int16(5000 * (1 + 0.3*math.Sin(2*math.Pi*3*t)) * v)
	}
	return out
}

func frameLevel(pcm []int16) float64 {
	var sum float64
	for _, s := range pcm {
		sum += float64(s) * float64(s)
	}
	return math.Sqrt(sum / float64(len(pcm)))
}

func largestStep(pcm []int16) int {
	var m int
	for i := 1; i < len(pcm); i++ {
		d := int(pcm[i]) - int(pcm[i-1])
		m = max(m, d, -d)
	}
	return m
}

// heardThroughMixer plays one participant's frames into a room of two and
// returns what the other hears, tick by tick. A tick in empty has nothing
// queued for it.
func heardThroughMixer(rate, ticks int, conceal bool, empty ...int) (heard [][]int16, src *Participant, tap *frameLog) {
	m := New(slog.New(slog.NewTextHandler(io.Discard, nil)), rate)
	// Comfort noise would fill a silent mix and hide what the source gave.
	m.SetComfortNoise(false)
	src = newTickParticipant(m, "src")
	if conceal {
		src.MarkConcealGaps()
	}
	tap = &frameLog{}
	src.tap = tap
	dst := newTickParticipant(m, "dst")
	for tick := range ticks {
		if !slices.Contains(empty, tick) {
			src.incoming <- encodeFrame(voicedFrame(rate, tick))
		}
		m.mixTick()
		heard = append(heard, decodeSamples(nil, <-dst.outgoing))
		<-src.outgoing
	}
	return heard, src, tap
}

func TestMixTickConcealsAnEmptyTick(t *testing.T) {
	for _, rate := range []int{8000, 16000, 48000} {
		t.Run(fmt.Sprint(rate), func(t *testing.T) {
			// Frame 12 starts on a rising slope: silence there would be a step.
			const lost = 12
			heard, src, tap := heardThroughMixer(rate, 20, true, lost)

			var clean []int16
			for f := range 20 {
				clean = append(clean, voicedFrame(rate, f)...)
			}
			bound := largestStep(clean)*3/2 + 50
			around := slices.Concat(heard[lost-1], heard[lost], heard[lost+1])
			if got := largestStep(around); got > bound {
				t.Errorf("largest step around the concealed tick is %d, above %d", got, bound)
			}
			before, got := frameLevel(heard[lost-1]), frameLevel(heard[lost])
			if got < before/2 || got > before*6/5 {
				t.Errorf("concealed tick has level %.0f after a frame at %.0f", got, before)
			}

			// The frame after the gap is cross-faded at its start only.
			n := len(heard[lost+1])
			if want := voicedFrame(rate, lost+1); !slices.Equal(heard[lost+1][n/2:], want[n/2:]) {
				t.Error("the second half of the frame after the gap was changed")
			}
			for f, pcm := range heard {
				if f != lost && f != lost+1 && !slices.Equal(pcm, voicedFrame(rate, f)) {
					t.Errorf("frame %d, away from the gap, was changed", f)
				}
			}

			// The participant's own tap hears what the room hears.
			if !bytes.Equal(tap.frames[lost], encodeFrame(heard[lost])) {
				t.Error("the tap did not get the concealed frame")
			}
			if !bytes.Equal(tap.frames[lost+1], encodeFrame(heard[lost+1])) {
				t.Error("the tap did not get the cross-faded frame")
			}
			if got := src.starved.Load(); got != 1 {
				t.Errorf("starved = %d, want 1", got)
			}
		})
	}
}

func TestMixTickConcealmentFadesToSilence(t *testing.T) {
	heard, _, _ := heardThroughMixer(16000, 20, true, 10, 11, 12, 13, 14)
	last := frameLevel(heard[9])
	for tick := 10; tick <= 12; tick++ {
		got := frameLevel(heard[tick])
		if got == 0 || got > last*6/5 {
			t.Errorf("tick %d of the gap has level %.0f after %.0f", tick, got, last)
		}
		last = got
	}
	// From 60 ms the substitute is silent.
	for tick := 13; tick <= 14; tick++ {
		if frameLevel(heard[tick]) != 0 {
			t.Errorf("tick %d, past 60 ms, is not silent", tick)
		}
	}
	// What follows silence is left alone, as after any pause.
	if !slices.Equal(heard[15], voicedFrame(16000, 15)) {
		t.Error("the first frame after a long gap was changed")
	}
}

func TestMixTickUnbrokenStreamIsUntouched(t *testing.T) {
	heard, src, _ := heardThroughMixer(16000, 20, true)
	for f, pcm := range heard {
		if !slices.Equal(pcm, voicedFrame(16000, f)) {
			t.Fatalf("frame %d was changed", f)
		}
	}
	if got := src.starved.Load(); got != 0 {
		t.Errorf("starved = %d, want 0", got)
	}
}

func TestMixTickEmptyTickIsSilentWithoutConcealGaps(t *testing.T) {
	heard, src, _ := heardThroughMixer(16000, 20, false, 12)
	if frameLevel(heard[12]) != 0 {
		t.Error("an empty tick was not silent")
	}
	if !slices.Equal(heard[13], voicedFrame(16000, 13)) {
		t.Error("the frame after an empty tick was changed")
	}
	if got := src.starved.Load(); got != 1 {
		t.Errorf("starved = %d, want 1", got)
	}
}
