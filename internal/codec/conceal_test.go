package codec

import (
	"math"
	"slices"
	"testing"
)

// voiced is a speech-like test signal: four harmonics of 110 Hz under a slow
// swell, so no two pitch periods are quite the same.
func voiced(rate, from, n int) []int16 {
	out := make([]int16, n)
	for i := range out {
		t := float64(from+i) / float64(rate)
		var v float64
		for h := 1; h <= 4; h++ {
			v += math.Sin(2*math.Pi*110*float64(h)*t+float64(h)) / float64(h*h)
		}
		out[i] = int16(5000 * (1 + 0.3*math.Sin(2*math.Pi*3*t)) * v)
	}
	return out
}

func maxStep(pcm []int16) int {
	var m int
	for i := 1; i < len(pcm); i++ {
		m = max(m, abs(int(pcm[i])-int(pcm[i-1])))
	}
	return m
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func rms(pcm []int16) float64 {
	var sum float64
	for _, s := range pcm {
		sum += float64(s) * float64(s)
	}
	return math.Sqrt(sum / float64(len(pcm)))
}

// concealRun plays frames of the test signal through a Concealer, with the
// frames from lostFrom to lostTo-1 lost, and returns the output.
func concealRun(rate, frames, lostFrom, lostTo int) (out, clean []int16) {
	frame := rate / 50
	c := NewConcealer(rate)
	clean = voiced(rate, 0, frames*frame)
	for f := range frames {
		if f >= lostFrom && f < lostTo {
			out = append(out, c.Conceal(nil, frame)...)
			continue
		}
		pcm := slices.Clone(clean[f*frame : (f+1)*frame])
		c.Good(pcm)
		out = append(out, pcm...)
	}
	return out, clean
}

func TestConcealerLeavesAnUnbrokenStreamAlone(t *testing.T) {
	for _, rate := range []int{8000, 48000} {
		out, clean := concealRun(rate, 50, 0, 0)
		if !slices.Equal(out, clean) {
			t.Errorf("%d Hz: a stream with no loss was changed", rate)
		}
	}
}

// The largest step between two samples anywhere in the output, gap edges
// included, stays within 1.5 times the largest step in the signal itself.
func TestConcealerHasNoStepAtEitherEdge(t *testing.T) {
	for _, rate := range []int{8000, 48000} {
		for _, lost := range []int{1, 2, 3} {
			frame := rate / 50
			clean := voiced(rate, 0, 30*frame)
			bound := maxStep(clean) * 3 / 2

			// Lose frames whose edges are far from a zero crossing, where
			// silence would break the bound at both ends.
			first := -1
			for f := 10; f < 25-lost; f++ {
				before, after := clean[f*frame-1], clean[(f+lost)*frame]
				if abs(int(before)) > 2*bound && abs(int(after)) > 2*bound {
					first = f
					break
				}
			}
			if first < 0 {
				t.Fatalf("%d Hz, %d lost: no gap with both edges above the bound", rate, lost)
			}

			out, _ := concealRun(rate, 30, first, first+lost)
			from, to := first*frame, (first+lost)*frame
			if got := maxStep(out[from-frame : to+frame]); got > bound {
				t.Errorf("%d Hz, %d lost: largest step %d, bound %d", rate, lost, got, bound)
			}
		}
	}
}

func TestConcealerFadesAndGoesSilent(t *testing.T) {
	for _, rate := range []int{8000, 48000} {
		frame := rate / 50
		block := rate / 100 // 10 ms
		out, clean := concealRun(rate, 30, 13, 19)
		gap := out[13*frame : 19*frame]
		ref := rms(clean[12*frame : 13*frame])

		if got := rms(gap[:block]); got < 0.7*ref || got > 1.3*ref {
			t.Errorf("%d Hz: first 10 ms at %.0f, want near the signal's %.0f", rate, got, ref)
		}
		prev := rms(gap[:block])
		for b := 1; b < 6; b++ {
			got := rms(gap[b*block : (b+1)*block])
			if got >= prev {
				t.Errorf("%d Hz: level rose from %.0f to %.0f in block %d", rate, prev, got, b)
			}
			prev = got
		}
		if got := rms(gap[5*block : 6*block]); got > 0.2*ref {
			t.Errorf("%d Hz: 50 to 60 ms still at %.0f of %.0f", rate, got, ref)
		}
		for i, s := range gap[6*block:] {
			if s != 0 {
				t.Fatalf("%d Hz: sample %d after 60 ms is %d, want silence", rate, i, s)
			}
		}
	}
}

func TestConcealerReturnsToTheSignal(t *testing.T) {
	for _, rate := range []int{8000, 48000} {
		frame := rate / 50
		for _, lost := range []int{1, 3} {
			out, clean := concealRun(rate, 30, 13, 13+lost)
			back := (13 + lost) * frame
			merge := 10 * rate / 1000 // the longest cross-fade
			if !slices.Equal(out[back+merge:], clean[back+merge:]) {
				t.Errorf("%d Hz, %d lost: output differs from the signal after the cross-fade", rate, lost)
			}
			if !slices.Equal(out[:13*frame], clean[:13*frame]) {
				t.Errorf("%d Hz, %d lost: output differs from the signal before the gap", rate, lost)
			}
			// The cross-fade ends on the signal: its last millisecond is close.
			ms := rate / 1000
			for i := back + merge - ms; i < back+merge; i++ {
				if d := abs(int(out[i]) - int(clean[i])); d > 1500 {
					t.Errorf("%d Hz, %d lost: sample %d is %d off the signal at the end of the cross-fade", rate, lost, i-back, d)
					break
				}
			}
		}
	}
}

// A steady tone is continued in phase: the substitute for one lost frame
// stays close to the tone that was lost.
func TestConcealerContinuesATone(t *testing.T) {
	const rate, frame = 8000, 160
	c := NewConcealer(rate)
	tone := func(f int) []int16 {
		pcm := make([]int16, frame)
		for i := range pcm {
			pcm[i] = int16(8000 * math.Sin(2*math.Pi*440*float64(f*frame+i)/rate))
		}
		return pcm
	}
	for f := range 10 {
		c.Good(tone(f))
	}
	got, want := c.Conceal(nil, frame), tone(10)
	// Only the first 10 ms: the fade starts there.
	var errSum float64
	for i := range frame / 2 {
		d := float64(got[i]) - float64(want[i])
		errSum += d * d
	}
	if e := math.Sqrt(errSum / (frame / 2)); e > 0.1*rms(want) {
		t.Errorf("substitute is %.0f rms away from the lost tone (%.0f rms)", e, rms(want))
	}
}

// Once the substitute has gone silent the stream has paused. What follows a
// pause is not touched.
func TestConcealerLeavesTheFrameAfterAPauseAlone(t *testing.T) {
	const rate, frame = 8000, 160
	out, clean := concealRun(rate, 30, 13, 17)
	if !slices.Equal(out[17*frame:], clean[17*frame:]) {
		t.Error("the frame after an 80 ms gap was changed")
	}
}

func TestConcealerWithNoHistoryIsSilent(t *testing.T) {
	c := NewConcealer(8000)
	for _, s := range c.Conceal(nil, 160) {
		if s != 0 {
			t.Fatal("concealment before any audio is not silence")
		}
	}
	// The first audio after that silence is not faded in.
	first := voiced(8000, 0, 160)
	c.Good(first)
	if !slices.Equal(first, voiced(8000, 0, 160)) {
		t.Fatal("the first frame after silence was changed")
	}
	c.Good(voiced(8000, 0, 160))
	c.Reset()
	for _, s := range c.Conceal(nil, 160) {
		if s != 0 {
			t.Fatal("concealment after Reset is not silence")
		}
	}
}

func TestG711DecodersConcealGaps(t *testing.T) {
	for _, ct := range []CodecType{CodecPCMU, CodecPCMA} {
		t.Run(ct.String(), func(t *testing.T) {
			enc, _ := NewEncoder(ct)
			dec, _ := NewDecoder(ct)
			gd, ok := dec.(GapDecoder)
			if !ok {
				t.Fatal("decoder does not implement GapDecoder")
			}
			if gd.InDTX() {
				t.Error("G.711 reports DTX")
			}
			clean := voiced(8000, 0, 30*160)
			var out []int16
			for f := range 30 {
				if f == 13 || f == 14 {
					pcm, err := gd.DecodeGap(nil)
					if err != nil || len(pcm) != 160 {
						t.Fatalf("DecodeGap: %d samples, error %v", len(pcm), err)
					}
					out = append(out, pcm...)
					continue
				}
				payload, _ := enc.Encode(clean[f*160 : (f+1)*160])
				pcm, err := dec.Decode(payload)
				if err != nil {
					t.Fatal(err)
				}
				out = append(out, pcm...)
			}
			// Companding adds a little to the steps of the signal itself.
			bound := maxStep(clean)*3/2 + 200
			if got := maxStep(out[12*160 : 16*160]); got > bound {
				t.Errorf("largest step around the gap %d, bound %d", got, bound)
			}
			if got := rms(out[13*160 : 14*160]); got < 0.5*rms(clean[12*160:13*160]) {
				t.Errorf("first concealed frame at %.0f rms, the signal was at %.0f", got, rms(clean[12*160:13*160]))
			}
		})
	}
}

func TestG711DecodersWithNoLossMatchThePlainDecoders(t *testing.T) {
	plain := map[CodecType]Decoder{CodecPCMU: &PCMUDecoder{}, CodecPCMA: &PCMADecoder{}}
	for ct, want := range plain {
		enc, _ := NewEncoder(ct)
		dec, _ := NewDecoder(ct)
		for f := range 100 {
			payload, _ := enc.Encode(voiced(8000, f*160, 160))
			a, _ := dec.Decode(payload)
			b, _ := want.Decode(payload)
			if !slices.Equal(a, b) {
				t.Fatalf("%s frame %d: output differs from the plain decoder", ct, f)
			}
		}
	}
}

// Extend runs the substitute on at the level it had reached at the end of the
// gap, joined with no step, and the cross-fade into the next frame follows it.
func TestConcealerExtendHoldsTheLevel(t *testing.T) {
	const rate = 16000
	c := NewConcealer(rate)
	tone := func(from, n int) []int16 {
		out := make([]int16, n)
		for i := range out {
			out[i] = int16(8000 * math.Sin(2*math.Pi*440*float64(from+i)/rate))
		}
		return out
	}
	n := 0
	for range 10 {
		c.Good(tone(n, 320))
		n += 320
	}
	gap := c.Conceal(nil, 320)
	head := c.Extend(nil, 128)
	next := tone(n+320+128, 320)
	c.Good(next)

	bound := maxStep(tone(0, 320))*3/2 + 50
	if got := maxStep(slices.Concat(gap, head, next)); got > bound {
		t.Errorf("largest step across the gap, the extension and the next frame is %d, above %d", got, bound)
	}
	end, held := rms(gap[240:]), rms(head)
	if held < end*0.9 || held > end*1.05 {
		t.Errorf("extension has level %.0f against %.0f at the end of the gap: the fade moved on", held, end)
	}
	if got := rms(next[:64]); got < held*0.85 {
		t.Errorf("the start of the cross-fade has level %.0f, under the extension's %.0f", got, held)
	}
}

func TestConcealerExtendOutsideAGapConceals(t *testing.T) {
	a, b := NewConcealer(8000), NewConcealer(8000)
	for f := range 10 {
		a.Good(voiced(8000, f*160, 160))
		b.Good(voiced(8000, f*160, 160))
	}
	if !slices.Equal(a.Extend(nil, 160), b.Conceal(nil, 160)) {
		t.Error("Extend with no gap in progress is not Conceal")
	}
}
