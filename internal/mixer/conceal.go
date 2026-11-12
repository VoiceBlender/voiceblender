package mixer

import (
	"encoding/binary"

	"github.com/VoiceBlender/voiceblender/internal/codec"
)

// gapConcealer stands in for a participant's frame on a tick that finds its
// queue empty. Only the mix tick uses it.
type gapConcealer struct {
	plc  *codec.Concealer
	pcm  []int16
	head []int16
	gap  bool // the last tick was concealed
}

// MarkConcealGaps has the mixer conceal a tick with nothing queued from this
// participant, in place of silence: it repeats the audio before the gap, fades
// it from 10 ms and is silent from 60 ms. It is for a live leg, whose packets
// can be lost or late. A source that stops fades out the same way.
//
// stale is how many samples at the start of each of the source's frames still
// carry its previous frame. A rate conversion puts them there: see
// ResampleOverlap.
func (p *Participant) MarkConcealGaps(stale int) {
	p.concealStale.Store(int32(stale))
	p.concealGaps.Store(true)
}

// ResampleOverlap is how many samples at the start of each frame converted
// from srcRate to dstRate still carry the input frame before it: the filter's
// delay, and the half of its length that reaches back past that.
func ResampleOverlap(srcRate, dstRate int) int {
	return 2 * NewPCMResampler(srcRate, dstRate).OutputLatency()
}

// frame returns what to play on a tick: the queued frame, or a substitute when
// queued is nil. The first frame after a gap is cross-faded in place with the
// end of the substitute.
func (c *gapConcealer) frame(queued []byte, rate, size, stale int) []byte {
	if c.plc == nil {
		c.plc = codec.NewConcealer(rate)
	}
	if queued == nil {
		c.pcm = c.plc.Conceal(c.pcm, size/2)
		c.gap = true
		out := make([]byte, size)
		putSamples(out, c.pcm)
		return out
	}
	c.pcm = decodeSamples(c.pcm, queued)
	if !c.gap {
		c.plc.Good(c.pcm)
		return queued
	}
	c.gap = false
	// After a gap the frame opens with the end of the source frame before the
	// gap, older than the substitute just played, smeared into the start of
	// the new audio. The substitute runs on over it at the level it had
	// reached, and the rest is cross-faded in.
	n := min(stale, len(c.pcm))
	c.head = c.plc.Extend(c.head, n)
	c.plc.Good(c.pcm[n:])
	copy(c.pcm, c.head)
	putSamples(queued, c.pcm)
	return queued
}

func putSamples(dst []byte, pcm []int16) {
	for i, s := range pcm {
		binary.LittleEndian.PutUint16(dst[i*2:], uint16(s))
	}
}
