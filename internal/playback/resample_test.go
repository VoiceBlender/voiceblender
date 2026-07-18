package playback

import (
	"bytes"
	"context"
	"encoding/binary"
	"log/slog"
	"math"
	"testing"
)

// toneSamples returns a freq-Hz sine of amplitude amp (0..1) at rate, starting
// at sample offset so a stream can be built in pieces without a phase jump.
func toneSamples(numSamples, offset int, freq float64, rate int, amp float64) []int16 {
	out := make([]int16, numSamples)
	for i := range out {
		out[i] = int16(amp * 32767 * math.Sin(2*math.Pi*freq*float64(offset+i)/float64(rate)))
	}
	return out
}

func tonePCM16(numSamples, offset int, freq float64, rate int, amp float64) []byte {
	s := toneSamples(numSamples, offset, freq, rate, amp)
	out := make([]byte, len(s)*2)
	for i, v := range s {
		binary.LittleEndian.PutUint16(out[i*2:], uint16(v))
	}
	return out
}

func decodePCM16(p []byte) []int16 {
	out := make([]int16, len(p)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(p[i*2:]))
	}
	return out
}

// toneAmplitude returns the amplitude of the freq-Hz component of x, in units
// of full scale (a full-scale sine reads ~1.0).
func toneAmplitude(x []int16, freq float64, rate int) float64 {
	if len(x) == 0 {
		return 0
	}
	var re, im float64
	w := 2 * math.Pi * freq / float64(rate)
	for i, s := range x {
		v := float64(s) / 32768.0
		re += v * math.Cos(w*float64(i))
		im += v * math.Sin(w*float64(i))
	}
	return 2 * math.Hypot(re, im) / float64(len(x))
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

// assertFramesContinuous asserts that a played-out continuous tone carries full
// energy in every frame past the stream's own lead-in. A resampler rebuilt per
// frame fades in from zero history on each one, collapsing each frame's leading
// samples while the stream's duration and overall shape still look right.
func assertFramesContinuous(t *testing.T, out []int16, frame int, freq float64, rate int, amp float64) {
	t.Helper()

	skip := 2 * frame
	if len(out) <= skip+frame {
		t.Fatalf("not enough output to measure: %d samples", len(out))
	}
	wantRMS := amp / math.Sqrt2

	for off := skip; off+frame <= len(out); off += frame {
		if got := rmsOf(out[off : off+frame]); got < wantRMS*0.9 {
			t.Fatalf("frame at sample %d has RMS %.4f, want >= %.4f (%.4f expected for a %g Hz sine at amplitude %g) — "+
				"filter history is not carrying across the frame boundary, so each frame leads with the filter's zero history",
				off, got, wantRMS*0.9, wantRMS, freq, amp)
		}
	}

	// No step a clean sine cannot make.
	limit := 2 * amp * 2 * math.Pi * freq / float64(rate)
	for i := skip + 1; i < len(out); i++ {
		d := math.Abs(float64(out[i])-float64(out[i-1])) / 32768.0
		if d > limit {
			t.Fatalf("step of %.4f at sample %d, want <= %.4f — the output has a discontinuity a %g Hz sine cannot produce",
				d, i, limit, freq)
		}
	}
}

// The tone is deliberately low: a gentle slope makes any zero-history gap
// re-injected at a frame boundary stand out sharply against it.
const (
	seamTone = 200.0
	seamAmp  = 0.9
)

// TestStreamRawPCM_NoSeamDiscontinuity drives a continuous tone through the
// real streamRawPCM loop — not through a resampler-shaped helper — and asserts
// the tone survives every 20 ms frame boundary intact.
//
// This is the guard for the resampler's ownership inside the stream loop. It
// fails if the resampler is built per frame, which re-zeroes the filter memory
// and stamps its zero history into the head of every frame.
func TestStreamRawPCM_NoSeamDiscontinuity(t *testing.T) {
	const (
		srcRate, dstRate = 16000, 8000
		frames           = 12
	)
	body := bytes.NewReader(tonePCM16(srcRate*frames*20/1000, 0, seamTone, srcRate, seamAmp))

	p := NewPlayer(slog.Default())
	var out bytes.Buffer
	if err := p.streamRawPCM(context.Background(), body, &out, srcRate, dstRate); err != nil {
		t.Fatalf("streamRawPCM: %v", err)
	}
	assertFramesContinuous(t, decodePCM16(out.Bytes()), dstRate*20/1000, seamTone, dstRate, seamAmp)
}

// TestStreamWAV_NoSeamDiscontinuity is the same guard on the WAV loop, which
// owns its own resampler and so needs its own proof.
func TestStreamWAV_NoSeamDiscontinuity(t *testing.T) {
	const (
		srcRate, dstRate = 8000, 16000
		frames           = 12
	)
	audio := tonePCM16(srcRate*frames*20/1000, 0, seamTone, srcRate, seamAmp)
	wav := buildWAV(1, 1, srcRate, 16, audio)

	p := NewPlayer(slog.Default())
	var out bytes.Buffer
	if err := p.streamWAV(context.Background(), bytes.NewReader(wav), &out, dstRate); err != nil {
		t.Fatalf("streamWAV: %v", err)
	}
	assertFramesContinuous(t, decodePCM16(out.Bytes()), dstRate*20/1000, seamTone, dstRate, seamAmp)
}

// TestStreamMP3_NoSeamDiscontinuity is the same guard on the MP3 loop, the
// third streaming site that owns a resampler and the one that had no proof.
//
// It drives streamMP3PCM rather than streamMP3 because there is no MP3 encoder
// on this box or in the module graph, and makeTestMP3 decodes to pure silence —
// which cannot distinguish a per-frame filter from a persistent one. The seam
// takes already-decoded frames, so the tone reaches the loop that matters.
// 44.1 kHz into an 8 kHz G.711 leg is the real prompt case.
func TestStreamMP3_NoSeamDiscontinuity(t *testing.T) {
	const (
		srcRate, dstRate = 44100, 8000
		frames           = 12
	)
	// go-mp3 always decodes to interleaved stereo. pcmToMono averages the two
	// channels, so an identical tone in L and R round-trips unchanged.
	mono := toneSamples(srcRate*frames*20/1000, 0, seamTone, srcRate, seamAmp)
	stereo := make([]byte, len(mono)*4)
	for i, s := range mono {
		binary.LittleEndian.PutUint16(stereo[i*4:], uint16(s))
		binary.LittleEndian.PutUint16(stereo[i*4+2:], uint16(s))
	}

	p := NewPlayer(slog.Default())
	var out bytes.Buffer
	if err := p.streamMP3PCM(context.Background(), bytes.NewReader(stereo), srcRate, int64(len(stereo)), &out, dstRate); err != nil {
		t.Fatalf("streamMP3PCM: %v", err)
	}
	assertFramesContinuous(t, decodePCM16(out.Bytes()), dstRate*20/1000, seamTone, dstRate, seamAmp)
}

// TestStreamRawPCM_ResamplerIsPerStream is the other half of the ownership
// contract, and it fails in the opposite direction from the seam guards.
//
// Player is long-lived and reused across playbacks. A resampler hung on it
// would keep its filter memory between them, so the tail of one prompt would
// bleed into the head of the next. Here a loud tone is played, then silence is
// played on the same Player: the silence must come out silent. A stream-scoped
// resampler starts with zero history and turns all-zero input into exact zeros;
// one carrying the previous playback's history rings on into this one.
func TestStreamRawPCM_ResamplerIsPerStream(t *testing.T) {
	const (
		srcRate, dstRate = 16000, 8000
		frames           = 4
	)
	p := NewPlayer(slog.Default())

	var loud bytes.Buffer
	tone := bytes.NewReader(tonePCM16(srcRate*frames*20/1000, 0, seamTone, srcRate, seamAmp))
	if err := p.streamRawPCM(context.Background(), tone, &loud, srcRate, dstRate); err != nil {
		t.Fatalf("streamRawPCM (tone): %v", err)
	}
	if rmsOf(decodePCM16(loud.Bytes())) == 0 {
		t.Fatal("the tone playback produced silence; the test proves nothing")
	}

	var quiet bytes.Buffer
	silence := bytes.NewReader(make([]byte, srcRate*frames*20/1000*2))
	if err := p.streamRawPCM(context.Background(), silence, &quiet, srcRate, dstRate); err != nil {
		t.Fatalf("streamRawPCM (silence): %v", err)
	}
	for i, s := range decodePCM16(quiet.Bytes()) {
		if s != 0 {
			t.Fatalf("silence playback emitted %d at sample %d — the previous playback's filter history is bleeding into this stream, "+
				"which is what happens when the resampler is owned by Player instead of the stream", s, i)
		}
	}
}
