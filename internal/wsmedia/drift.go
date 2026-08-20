package wsmedia

import (
	"encoding/binary"
	"math"

	"github.com/VoiceBlender/voiceblender/internal/speaking"
)

// maxConcealFrames bounds how many frames may be invented back to back before
// the reader stops pretending and lets the mixer see the gap. Two frames is the
// usual packet-loss-concealment bound; past that a repeated frame stops sounding
// like speech and starts sounding like a 50 Hz buzz.
const maxConcealFrames = 2

// maxInsertRun caps consecutive duplicated frames. One is enough to lift the
// level when the producer is merely slow; needing more in a row means it has
// stopped altogether, and stretching its last silence forever would hide that
// from the mixer's starvation counters.
const maxInsertRun = 2

// concealCrossfadeMs is how much of a concealed frame is blended with the tail
// of the frame it repeats. Splicing a frame onto itself steps at the seam
// because its last sample does not continue into its first; a couple of
// milliseconds of overlap removes that step without needing pitch analysis.
const concealCrossfadeMs = 2

// DriftStats counts the corrections the playout buffer made to keep the
// producer's clock aligned with the mixer's.
type DriftStats struct {
	// Underruns is how many times the lead ran dry and had to be rebuilt.
	Underruns int64
	// Trimmed and Inserted count frames removed or duplicated during a pause,
	// which is the free way to absorb drift: nothing audible changes.
	Trimmed  int64
	Inserted int64
	// Concealed counts invented frames emitted mid-speech, when the deficit
	// could not wait for a pause. These are audible in principle, unlike
	// Trimmed/Inserted.
	Concealed int64
}

// frameIsQuiet reports whether a frame of little-endian PCM16 sits below the
// speech floor, using the same threshold as the speaking detector so "quiet"
// means one thing across the codebase.
func frameIsQuiet(frame []byte) bool {
	n := len(frame) / 2
	if n == 0 {
		return true
	}
	var sum float64
	for i := 0; i < n; i++ {
		s := float64(int16(binary.LittleEndian.Uint16(frame[i*2:])))
		sum += s * s
	}
	return math.Sqrt(sum/float64(n)) < speaking.Threshold
}

// writeConceal fills dst with last, ramped from gStart to gEnd and cross-faded
// at the seam. Successive calls with a falling gain fade the repeat out, so the
// stream reaches true silence smoothly instead of stepping into it.
func writeConceal(dst, last []byte, gStart, gEnd float64, sampleRate int) {
	n := len(dst) / 2
	if n == 0 || len(last) < len(dst) {
		clear(dst)
		return
	}
	xf := concealCrossfadeMs * sampleRate / 1000
	if xf > n {
		xf = n
	}
	tail := n - xf
	for i := 0; i < n; i++ {
		s := float64(int16(binary.LittleEndian.Uint16(last[i*2:])))
		if i < xf {
			// Blend out of the previous frame's tail and into its head.
			prev := float64(int16(binary.LittleEndian.Uint16(last[(tail+i)*2:])))
			w := float64(i) / float64(xf)
			s = prev*(1-w) + s*w
		}
		g := gStart + (gEnd-gStart)*float64(i)/float64(n)
		binary.LittleEndian.PutUint16(dst[i*2:], uint16(clampPCM(s*g)))
	}
}

func clampPCM(v float64) int16 {
	switch {
	case v > math.MaxInt16:
		return math.MaxInt16
	case v < math.MinInt16:
		return math.MinInt16
	}
	return int16(v)
}

// concealGains returns the ramp for the run'th consecutive concealed frame,
// falling to zero as the budget runs out.
func concealGains(run int) (float64, float64) {
	g := func(r int) float64 {
		if r >= maxConcealFrames {
			return 0
		}
		return float64(maxConcealFrames-r) / float64(maxConcealFrames)
	}
	return g(run - 1), g(run)
}
