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
		src.MarkConcealGaps(0)
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

func toneAt(rate, from, n int) []int16 {
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(8000 * math.Sin(2*math.Pi*440*float64(from+i)/float64(rate)))
	}
	return out
}

// A leg reaches the mixer through a rate conversion whose filter delay puts
// the end of each leg frame at the start of the next mix frame. When a leg
// frame is lost, the mix frame after the gap opens with the end of the frame
// before it: older audio than the substitute, and out of step with it. The
// substitute must run on over that stale start before the cross-fade.
func TestMixTickConcealmentRunsOverAStaleStart(t *testing.T) {
	const (
		rate  = 16000
		n     = rate / 50
		stale = 64 // 8 to 16 kHz, as ResampleDelay gives
		lost  = 12
	)
	if got := ResampleDelay(8000, rate); got != stale {
		t.Fatalf("ResampleDelay(8000, %d) = %d, want %d", rate, got, stale)
	}
	// Mix frame k is the tone from k*n-stale. Leg frame lost never arrives,
	// so mix frame lost+1 is its stale start, then frame lost+1 onwards.
	frameFor := func(k int) []int16 {
		if k == lost+1 {
			return slices.Concat(toneAt(rate, lost*n-stale, stale), toneAt(rate, (lost+1)*n, n-stale))
		}
		return toneAt(rate, k*n-stale, n)
	}
	play := func(stale int) [][]int16 {
		m := New(slog.New(slog.NewTextHandler(io.Discard, nil)), rate)
		m.SetComfortNoise(false)
		src := newTickParticipant(m, "src")
		src.MarkConcealGaps(stale)
		dst := newTickParticipant(m, "dst")
		var heard [][]int16
		for tick := range 20 {
			if tick != lost {
				src.incoming <- encodeFrame(frameFor(tick))
			}
			m.mixTick()
			heard = append(heard, decodeSamples(nil, <-dst.outgoing))
			<-src.outgoing
		}
		return heard
	}

	bound := largestStep(toneAt(rate, 0, n))*3/2 + 50
	heard := play(stale)
	around := slices.Concat(heard[lost-1], heard[lost], heard[lost+1])
	if got := largestStep(around); got > bound {
		t.Errorf("largest step around the gap is %d, above %d", got, bound)
	}
	// By the end of the gap the substitute has faded to 72%, so the cross-fade
	// starts from there.
	tone := frameLevel(toneAt(rate, 0, n))
	for i := 0; i+16 <= len(heard[lost+1]); i += 16 {
		if got := frameLevel(heard[lost+1][i : i+16]); got < tone*0.6 {
			t.Errorf("1 ms block %d after the gap has level %.0f, under 60%% of the tone's %.0f", i/16, got, tone)
		}
	}
	// The stale start, then a 10 ms cross-fade; after that the frame is the
	// source's own.
	if from := stale + 10*rate/1000; !slices.Equal(heard[lost+1][from:], frameFor(lost + 1)[from:]) {
		t.Errorf("the frame after the gap was changed after sample %d", from)
	}

	// Without the stale count the old start is cross-faded into, and the jump
	// to the next frame inside it shows.
	if got := largestStep(slices.Concat(heard[lost], play(0)[lost+1])); got <= bound {
		t.Errorf("with no stale count the largest step is %d, within %d: the test does not show the fault", got, bound)
	}
}
