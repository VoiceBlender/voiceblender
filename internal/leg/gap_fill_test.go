package leg

import (
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
