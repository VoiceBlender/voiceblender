package mixer

import (
	"encoding/binary"

	"github.com/VoiceBlender/voiceblender/internal/codec"
)

// gapConcealer stands in for a participant's frame on a tick that finds its
// queue empty. Only the mix tick uses it.
type gapConcealer struct {
	plc *codec.Concealer
	pcm []int16
	gap bool // the last tick was concealed
}

// MarkConcealGaps has the mixer conceal a tick with nothing queued from this
// participant, in place of silence: it repeats the audio before the gap, fades
// it from 10 ms and is silent from 60 ms. It is for a live leg, whose packets
// can be lost or late. A source that stops fades out the same way.
func (p *Participant) MarkConcealGaps() {
	p.concealGaps.Store(true)
}

// frame returns what to play on a tick: the queued frame, or a substitute when
// queued is nil. The first frame after a gap is cross-faded in place with the
// end of the substitute.
func (c *gapConcealer) frame(queued []byte, rate, size int) []byte {
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
	c.plc.Good(c.pcm)
	if c.gap {
		c.gap = false
		putSamples(queued, c.pcm)
	}
	return queued
}

func putSamples(dst []byte, pcm []int16) {
	for i, s := range pcm {
		binary.LittleEndian.PutUint16(dst[i*2:], uint16(s))
	}
}
