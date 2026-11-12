package mixer

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"math"
	"slices"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/codec"
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

// throughConversion plays a 440 Hz PCMU leg into a 16 kHz room through the
// real rate conversion, with leg frame lost missing, and returns what the
// other party hears, tick by tick, and the converted frames that were queued.
// The leg side does what a SIP leg does when it passes over a hole: its
// decoder conceals the lost frame without playing it, cross-fades the start
// of the next frame from its own substitute, and the gap filler then
// cross-fades the first 4 ms in from that substitute (see gapFiller.joinTo).
func throughConversion(stale, lost, ticks int) (heard, queued [][]int16) {
	const rate = 16000
	rs := NewPCMResampler(8000, rate)
	dec := codec.NewConcealer(8000)
	m := New(slog.New(slog.NewTextHandler(io.Discard, nil)), rate)
	m.SetComfortNoise(false)
	src := newTickParticipant(m, "src")
	src.MarkConcealGaps(stale)
	dst := newTickParticipant(m, "dst")
	var join []int16
	for tick := range ticks {
		var frame []int16
		if tick == lost {
			join = dec.Conceal(nil, 160)
		} else {
			leg := make([]int16, 160)
			for i := range leg {
				leg[i] = int16(8000 * math.Sin(2*math.Pi*440*float64(tick*160+i)/8000))
			}
			dec.Good(leg)
			n := len(join) / 5
			for i := range n {
				w := float32(i+1) / float32(n+1)
				leg[i] = int16(float32(join[i])*(1-w) + float32(leg[i])*w)
			}
			join = nil
			frame = rs.ResampleSamples(leg)
			src.incoming <- encodeFrame(frame)
		}
		queued = append(queued, frame)
		m.mixTick()
		heard = append(heard, decodeSamples(nil, <-dst.outgoing))
		<-src.outgoing
	}
	return heard, queued
}

// lowest5ms is the lowest level of any 5 ms window, sliding by 1 ms.
func lowest5ms(pcm []int16, rate int) float64 {
	w, step := rate/200, rate/1000
	low := math.Inf(1)
	for i := 0; i+w <= len(pcm); i += step {
		low = min(low, frameLevel(pcm[i:i+w]))
	}
	return low
}

// A leg reaches the mixer through a rate conversion whose filter delay (4 ms
// here) puts the end of each leg frame at the start of the next mix frame.
// When a leg frame is lost and the mixer has concealed its tick, the mix frame
// after the gap opens with that stale end and then the leg's own 4 ms join,
// which also carries on from before the gap. Both are older than the mixer's
// substitute and out of step with it. The substitute must run on over all
// 8 ms, at the level it had reached, before the cross-fade, or the level dips.
func TestMixTickConcealmentAllowsForTheRateConversion(t *testing.T) {
	const rate = 16000
	overlap := ResampleOverlap(8000, rate)
	if overlap != 128 {
		t.Fatalf("ResampleOverlap(8000, %d) = %d, want 128", rate, overlap)
	}
	// worst is the lowest 5 ms level around the gap, against the tone, over
	// five loss positions: 20 ms is 8.8 cycles of the tone, so five positions
	// cover every phase it can be lost at.
	worst := func(stale int) float64 {
		low := math.Inf(1)
		for lost := 10; lost < 15; lost++ {
			heard, queued := throughConversion(stale, lost, lost+3)
			around := slices.Concat(heard[lost-1], heard[lost], heard[lost+1])
			low = min(low, lowest5ms(around, rate)/frameLevel(heard[5]))
			if stale != overlap {
				continue
			}
			if bound := largestStep(heard[5])*3/2 + 50; largestStep(around) > bound {
				t.Errorf("lost %d: largest step around the gap is %d, above %d", lost, largestStep(around), bound)
			}
			// The overlap, then the cross-fade (8 ms after one lost frame);
			// after that the frame is the converted source's own.
			if from := overlap + 8*rate/1000; !slices.Equal(heard[lost+1][from:], queued[lost+1][from:]) {
				t.Errorf("lost %d: the frame after the gap was changed after sample %d", lost, from)
			}
		}
		return low
	}

	// The substitute is at 77% by the end of the gap: no lower than that, and
	// above what the loss analysis counts as a dip (a 5 ms block under 70%).
	if got := worst(overlap); got < 0.75 {
		t.Errorf("lowest 5 ms around the gap is at %.0f%% of the tone", 100*got)
	}
	if got := worst(0); got >= 0.7 {
		t.Errorf("with no allowance the lowest 5 ms is at %.0f%% of the tone: the test does not show the fault", 100*got)
	}
}
