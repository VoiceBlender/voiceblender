package leg

import (
	"slices"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/codec"
)

type fakeGapDecoder struct {
	dtx  bool
	gaps int
}

func (d *fakeGapDecoder) Decode(data []byte) ([]int16, error) { return make([]int16, 160), nil }
func (d *fakeGapDecoder) Reset()                              {}
func (d *fakeGapDecoder) InDTX() bool                         { return d.dtx }
func (d *fakeGapDecoder) DecodeGap(dst []int16) ([]int16, error) {
	d.gaps++
	return make([]int16, 160), nil
}

func newTestGapFiller(dtx bool) (*gapFiller, *fakeGapDecoder, *int) {
	dec := &fakeGapDecoder{dtx: dtx}
	emitted := new(int)
	return newGapFiller(dec, 160), dec, emitted
}

func TestGapFillerOnlyForGapDecoders(t *testing.T) {
	if g := newGapFiller(&codec.PCMUDecoder{}, 160); g != nil {
		t.Fatal("gap filler built for a decoder without DecodeGap")
	}
}

func TestGapFillerConcealsShortHoleOnArrival(t *testing.T) {
	g, dec, emitted := newTestGapFiller(false)
	emit := func(uint16, []int16) { *emitted++ }
	now := time.Now()

	if g.before(1, 1000, 0, nil, emit) {
		t.Fatal("first packet dropped")
	}
	g.after(1, 1000, 160, now)

	// Two frames (1160, 1320) lost before 1480.
	if g.before(1, 1480, 0, nil, emit) {
		t.Fatal("packet after a hole dropped")
	}
	if dec.gaps != 2 || *emitted != 2 {
		t.Fatalf("filled %d, emitted %d, want 2 and 2", dec.gaps, *emitted)
	}
	g.after(1, 1480, 160, now)

	// Speech, not DTX: no timer fills.
	if _, ok := g.deadline(); ok {
		t.Error("timer fill scheduled outside DTX")
	}
}

func TestGapFillerDropsLateAndDuplicatePackets(t *testing.T) {
	g, _, _ := newTestGapFiller(false)
	emit := func(uint16, []int16) {}
	g.after(1, 1000, 160, time.Now())
	if !g.before(1, 1000, 0, nil, emit) {
		t.Error("duplicate packet not dropped")
	}
	g.after(1, 1160, 160, time.Now())
	if !g.before(1, 1000, 0, nil, emit) {
		t.Error("late packet not dropped")
	}
}

func TestGapFillerResyncsOnDiscontinuity(t *testing.T) {
	g, dec, _ := newTestGapFiller(false)
	emit := func(uint16, []int16) {}
	g.after(1, 1000, 160, time.Now())

	// A long jump is not concealed.
	if g.before(1, 1000+160*50, 0, nil, emit) || dec.gaps != 0 {
		t.Fatalf("long jump: dropped or filled %d frames", dec.gaps)
	}
	// A new SSRC restarts the timeline, even backwards.
	g.after(1, 1000, 160, time.Now())
	if g.before(2, 10, 0, nil, emit) || dec.gaps != 0 {
		t.Fatalf("new SSRC: dropped or filled %d frames", dec.gaps)
	}
}

func TestGapFillerComfortNoiseInDTX(t *testing.T) {
	g, dec, emitted := newTestGapFiller(true)
	emit := func(uint16, []int16) { *emitted++ }
	start := time.Now()
	g.after(1, 0, 160, start)

	at, ok := g.deadline()
	if !ok {
		t.Fatal("no comfort noise scheduled in DTX")
	}
	if want := start.Add(gapFrame + gapSlack); !at.Equal(want) {
		t.Errorf("first fill at %v, want %v", at.Sub(start), want.Sub(start))
	}

	// 140 ms after the SID: six intervals (20..120 ms) are due with the slack.
	g.tick(start.Add(140*time.Millisecond), emit)
	if *emitted != 6 {
		t.Fatalf("emitted %d comfort-noise frames by 140 ms, want 6", *emitted)
	}

	// The next SID update arrives on time for interval 8 (1280): interval 7
	// is filled on arrival and the SID is decoded.
	if g.before(1, 8*160, 0, nil, emit) {
		t.Fatal("on-time SID update dropped")
	}
	if *emitted != 7 {
		t.Errorf("emitted %d frames by the SID update, want 7", *emitted)
	}
	g.after(1, 8*160, 160, start.Add(160*time.Millisecond))

	// Without any further packet, comfort noise stops after gapMaxFill frames.
	dec.gaps = 0
	g.tick(start.Add(time.Hour), emit)
	if dec.gaps != gapMaxFill {
		t.Errorf("filled %d frames with no packets, want %d", dec.gaps, gapMaxFill)
	}
	if _, ok := g.deadline(); ok {
		t.Error("comfort noise still scheduled after the cap")
	}
}

func TestGapFillerStopsWhenSpeechResumes(t *testing.T) {
	g, dec, _ := newTestGapFiller(true)
	g.after(1, 0, 160, time.Now())
	dec.dtx = false
	if _, ok := g.deadline(); ok {
		t.Error("comfort noise scheduled after speech resumed")
	}
}

// constGapDecoder conceals every lost frame with the same value.
type constGapDecoder struct {
	fakeGapDecoder
	value int16
}

func (d *constGapDecoder) DecodeGap(dst []int16) ([]int16, error) {
	d.gaps++
	out := make([]int16, 160)
	for i := range out {
		out[i] = d.value
	}
	return out, nil
}

// With the jitter buffer off, a fill is played only while a frame is waiting
// to be played: otherwise its tick has gone, or will before the fill gets
// there, and the fill would leave the stream 20 ms behind. The decoder is told
// of every lost frame either way.
func TestGapFillerPassesOverAHoleWhenNothingIsQueued(t *testing.T) {
	for _, tc := range []struct {
		name      string
		queued    func() int
		hole      int
		wantFills int
	}{
		{"no mixer", nil, 2, 2},
		{"mixer, not attached", func() int { return -1 }, 2, 2},
		{"one frame waiting", func() int { return 1 }, 2, 2},
		{"nothing waiting, one lost", func() int { return 0 }, 1, 0},
		{"nothing waiting, three lost", func() int { return 0 }, 3, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, dec, emitted := newTestGapFiller(false)
			g.queued = tc.queued
			emit := func(uint16, []int16) { *emitted++ }

			g.before(1, 1000, 0, nil, emit)
			g.after(1, 1000, 160, time.Now())
			if g.before(1, uint32(1000+160*(tc.hole+1)), 0, nil, emit) {
				t.Fatal("packet after a hole dropped")
			}
			if dec.gaps != tc.hole {
				t.Errorf("decoder told of %d lost frames, want %d", dec.gaps, tc.hole)
			}
			if *emitted != tc.wantFills {
				t.Errorf("%d fills emitted, want %d", *emitted, tc.wantFills)
			}
		})
	}
}

// The frame after a hole that was passed over follows the frame before it
// directly. Its start is cross-faded in from the substitute for the hole's
// first frame, which carries on from the frame before.
func TestGapFillerJoinsTheFramesEitherSideOfAPassedOverHole(t *testing.T) {
	dec := &constGapDecoder{value: 1000}
	g := newGapFiller(dec, 160)
	g.queued = func() int { return 0 }
	emit := func(uint16, []int16) { t.Error("a fill was emitted") }

	g.before(1, 1000, 0, nil, emit)
	g.after(1, 1000, 160, time.Now())
	g.before(1, 1000+160*3, 0, nil, emit)
	g.after(1, 1000+160*3, 160, time.Now())
	next := slices.Repeat([]int16{-1000}, 160)
	g.joinTo(next)

	if next[0] < 900 {
		t.Errorf("first sample after the hole is %d, want close to the substitute's 1000", next[0])
	}
	for i := 1; i < 32; i++ {
		if next[i] > next[i-1] {
			t.Fatalf("the cross-fade turns back at sample %d", i)
		}
	}
	if !slices.Equal(next[32:], slices.Repeat([]int16{-1000}, 128)) {
		t.Error("the frame was changed after its first 4 ms")
	}

	// The join is used once.
	again := slices.Repeat([]int16{-1000}, 160)
	g.joinTo(again)
	if again[0] != -1000 {
		t.Error("a second frame was cross-faded")
	}
}

// A join is for the frame that follows the hole. If a later hole is filled,
// the frame after it follows the fill, and the old join must not reach it.
func TestGapFillerDropsAJoinOnceAFillIsPlayed(t *testing.T) {
	dec := &constGapDecoder{value: 1000}
	g := newGapFiller(dec, 160)
	queued := 0
	g.queued = func() int { return queued }
	var fills int
	emit := func(uint16, []int16) { fills++ }

	g.before(1, 1000, 0, nil, emit)
	g.after(1, 1000, 160, time.Now())
	g.before(1, 1000+160*2, 0, nil, emit)
	// That packet failed to decode, so its own slot is a hole too. By the
	// next packet a frame is waiting, and the slot is filled.
	queued = 1
	g.before(1, 1000+160*3, 0, nil, emit)
	if fills != 1 {
		t.Fatalf("%d fills played, want 1", fills)
	}
	next := slices.Repeat([]int16{-1000}, 160)
	g.joinTo(next)
	if next[0] != -1000 {
		t.Error("the join from the first hole was applied after a fill")
	}
}
