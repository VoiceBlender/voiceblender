package leg

import (
	"encoding/binary"

	"github.com/VoiceBlender/voiceblender/internal/codec"
)

// playoutConcealer conceals the ticks a jitter buffer has nothing to play
// for. It works on the PCM the buffer plays out: the decoder ran before the
// buffer, in arrival order, so its own concealment cannot be used here.
type playoutConcealer struct {
	plc     *codec.Concealer
	rate    int
	gap     bool
	samples []int16
}

func concealedAtPlayout(ct codec.CodecType) bool {
	switch ct {
	case codec.CodecPCMU, codec.CodecPCMA, codec.CodecOpus:
		return true
	}
	return false
}

// pop returns the stream's next 20 ms: the buffer's frame when it has one,
// concealment or silence when it has not.
func (p *playoutConcealer) pop(s *mediaStream) []byte {
	p.follow(s.live())
	pcm, ok := s.jb.Pop()
	if !ok {
		return p.conceal(s.jbFrameBytes)
	}
	p.good(pcm)
	return pcm
}

// follow points the concealer at the live codec. Codecs it does not cover
// keep silence.
func (p *playoutConcealer) follow(lc *liveCodec) {
	if lc == nil || !concealedAtPlayout(lc.codecType) {
		p.plc = nil
		return
	}
	if rate := lc.sampleRate(); p.plc == nil || rate != p.rate {
		p.plc, p.rate, p.gap = codec.NewConcealer(rate), rate, false
	}
}

// good records a frame the buffer played. After a concealed gap the start of
// pcm is changed in place, to join it to the substitute.
func (p *playoutConcealer) good(pcm []byte) {
	if p.plc == nil {
		return
	}
	p.samples = bytesToSamplesInto(p.samples, pcm)
	p.plc.Good(p.samples)
	if p.gap {
		p.gap = false
		for i, v := range p.samples {
			binary.LittleEndian.PutUint16(pcm[2*i:], uint16(v))
		}
	}
}

// conceal returns n bytes of PCM for a tick with nothing to play.
func (p *playoutConcealer) conceal(n int) []byte {
	if p.plc == nil {
		return make([]byte, n)
	}
	p.gap = true
	p.samples = p.plc.Conceal(p.samples, n/2)
	return samplesToBytes(p.samples)
}
