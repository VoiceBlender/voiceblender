package api

import (
	"context"
	"encoding/binary"
	"math"
	"testing"
	"time"
)

func decodePCM16(p []byte) []int16 {
	out := make([]int16, len(p)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(p[i*2:]))
	}
	return out
}

func rmsOf(x []int16) float64 {
	if len(x) == 0 {
		return 0
	}
	var sum float64
	for _, s := range x {
		v := float64(s) / 32768.0
		sum += v * v
	}
	return math.Sqrt(sum / float64(len(x)))
}

// assertFramesWhole asserts that a resampled continuous tone carries full
// energy in every frame past the stream's own lead-in, and never steps harder
// than the tone itself can. A resampler rebuilt per frame fades in from zero
// history on each one, collapsing that frame's RMS.
func assertFramesWhole(t *testing.T, out []int16, frame int, toneHz, rate int) {
	t.Helper()

	skip := 2 * frame
	if len(out) <= skip+frame {
		t.Fatalf("not enough output to measure: %d samples", len(out))
	}
	// sineFrame/contSineFrame use an amplitude of 16000 of 32768.
	const amp = 16000.0 / 32768.0
	wantRMS := amp / math.Sqrt2

	for off := skip; off+frame <= len(out); off += frame {
		if got := rmsOf(out[off : off+frame]); got < wantRMS*0.9 {
			t.Fatalf("frame at sample %d has RMS %.4f, want >= %.4f — filter history is not carrying across the frame boundary, "+
				"so each frame leads with the resampler's zero history instead of audio",
				off, got, wantRMS*0.9)
		}
	}

	limit := 2 * amp * 2 * math.Pi * float64(toneHz) / float64(rate)
	for i := skip + 1; i < len(out); i++ {
		d := math.Abs(float64(out[i])-float64(out[i-1])) / 32768.0
		if d > limit {
			t.Fatalf("step of %.4f at sample %d, want <= %.4f — the output has a discontinuity a %d Hz sine cannot produce",
				d, i, limit, toneHz)
		}
	}
}

// seamToneHz is deliberately low: a gentle slope makes any zero-history gap
// re-injected at a frame boundary stand out sharply against it.
const seamToneHz = 200

// TestLegPlaybackWriter_NoSeamDiscontinuity drives many consecutive 20 ms
// frames of a continuous sine through the real legPlaybackWriter.Write and
// asserts every frame comes out whole.
//
// This is the guard that per-frame resampler construction fails, and it is the
// only one on this path that can. Constructing a resampler zeroes its whole
// filter state, so one built inside Write emits that zero history as the
// leading samples of every 20 ms frame — a gap at 50 Hz, and measurably worse
// than the linear interpolation this item replaced. It slips past every other
// assert here: the duration mapping is unaffected (the counts come out exactly
// right), and the alias attenuation looks excellent, because the filter really
// is filtering — it is just restarting on every frame.
func TestLegPlaybackWriter_NoSeamDiscontinuity(t *testing.T) {
	const (
		srcRate, dstRate = 16000, 8000
		frames           = 25
	)
	l := &playbackTestLeg{id: "leg-1", sampleRate: dstRate}
	w := &legPlaybackWriter{
		legID:        l.id,
		leg:          l,
		directWriter: l.AudioWriter(),
		roomMgr:      newPlaybackRoomMgr(t),
		srcRate:      srcRate,
	}
	for i := range frames {
		if _, err := w.Write(contSineFrame(srcRate, seamToneHz, 20, i)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	assertFramesWhole(t, decodePCM16(l.directBytes.Bytes()), dstRate*20/1000, seamToneHz, dstRate)
}

// TestLegPlaybackWriter_RateFlipReusesResampler covers the mid-stream
// destination-rate flip: a leg that joins a room mid-playback switches from its
// own native rate to the room mixer's rate, and can switch back.
//
// Each destination rate must get its own resampler, and a flip must reuse the
// cached one rather than rebuild it. Rebuilding on each flip is per-frame
// construction wearing a different hat — it re-zeroes the filter memory, so the
// frame after every flip leads with zeros instead of audio.
func TestLegPlaybackWriter_RateFlipReusesResampler(t *testing.T) {
	const (
		srcRate  = 16000
		legRate  = 8000
		roomRate = 48000
	)
	rmMgr := newPlaybackRoomMgr(t)
	rm, err := rmMgr.Create("room-flip", "", roomRate)
	if err != nil {
		t.Fatalf("create room: %v", err)
	}
	t.Cleanup(func() { _ = rmMgr.Delete(rm.ID) })

	l := &playbackTestLeg{id: "flip-leg", sampleRate: legRate}

	// The inject path only exists once the leg is a mixer participant.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rm.Mixer().SetComfortNoise(false)
	rm.Mixer().AddParticipant(l.id, &silenceReader{ctx: ctx}, &captureWriter{})
	rm.Mixer().Start()
	t.Cleanup(func() {
		rm.Mixer().RemoveParticipant(l.id)
		rm.Mixer().Stop()
	})
	time.Sleep(40 * time.Millisecond)

	w := &legPlaybackWriter{
		legID:        l.id,
		leg:          l,
		directWriter: l.AudioWriter(),
		roomMgr:      rmMgr,
		srcRate:      srcRate,
	}

	write := func(phase string, n int, frame *int) {
		t.Helper()
		for range n {
			if _, err := w.Write(contSineFrame(srcRate, seamToneHz, 20, *frame)); err != nil {
				t.Fatalf("Write (%s): %v", phase, err)
			}
			*frame++
		}
	}

	// Out of any room: srcRate -> legRate, straight to the leg.
	frame := 0
	write("pre-room", 5, &frame)
	if len(w.resamplers) != 1 {
		t.Fatalf("after the direct-path frames the writer holds %d resampler(s), want 1", len(w.resamplers))
	}
	legRS := w.resamplers[legRate]
	if legRS == nil {
		t.Fatalf("no resampler cached for the leg rate %d", legRate)
	}

	// Join the room: the destination flips to srcRate -> roomRate.
	l.SetRoomID(rm.ID)
	write("in-room", 5, &frame)
	roomRS := w.resamplers[roomRate]
	if roomRS == nil {
		t.Fatalf("no resampler cached for the room rate %d", roomRate)
	}
	if roomRS == legRS {
		t.Fatal("the room rate reused the leg rate's resampler — each destination rate needs its own filter history")
	}

	// Leave the room: back to srcRate -> legRate. The original resampler must
	// come back, not a fresh one.
	l.SetRoomID("")
	write("post-room", 5, &frame)
	if got := w.resamplers[legRate]; got != legRS {
		t.Error("flipping back to the leg rate rebuilt its resampler instead of reusing the cached one, re-zeroing its filter memory")
	}
	if len(w.resamplers) != 2 {
		t.Errorf("writer holds %d resamplers, want exactly 2 (one per destination rate)", len(w.resamplers))
	}
}
