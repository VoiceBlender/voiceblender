package audiofilter

import (
	"encoding/binary"
	"github.com/VoiceBlender/voiceblender/internal/mixer"
	"io"
	"sync"
)

// Writer is the push-mode counterpart of Reader: audio is fed in with Write
// rather than pulled through Read. It exists for the observation paths that
// run before a leg joins a room — answering-machine detection starts during
// ringing, when there is no mixer participant and so no Reader to sit behind.
//
// Those paths are taps, which are io.Writers, so the chain has to be driven
// from the write side.
type Writer struct {
	mu  sync.Mutex
	dst io.Writer
	c   *chain

	carryArr [1]byte
	hasCarry bool

	inSamp []int16
	rsIn   []int16
	acc    []int16
	accN   int
	block  []int16
	rsOut  []int16
	out    []byte
}

// NewWriter filters audio written to it and passes the result to dst. With no
// filters it degrades to the plain resampling writer, so a caller pays nothing
// for the option.
func NewWriter(dst io.Writer, srcRate, dstRate int, specs []Spec) (io.WriteCloser, error) {
	if len(specs) == 0 {
		return nopCloser{mixer.NewResampleWriter(dst, srcRate, dstRate)}, nil
	}
	c, err := buildChain(srcRate, dstRate, specs)
	if err != nil {
		return nil, err
	}
	return &Writer{
		dst:    dst,
		c:      c,
		inSamp: make([]int16, 0, c.hop),
		rsIn:   make([]int16, 0, c.hop+64),
		acc:    make([]int16, c.hop*2),
		block:  make([]int16, c.hop),
		rsOut:  make([]int16, 0, c.hop*dstRate/c.work+64),
		out:    make([]byte, 0, c.hop*4),
	}, nil
}

// Close releases the stages, handing any denoise state back to the pool. It
// does not close dst: the chain does not own it.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.c.Close()
}

func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	total := len(p)
	w.inSamp = w.inSamp[:0]
	if w.hasCarry && len(p) > 0 {
		w.inSamp = append(w.inSamp, int16(uint16(w.carryArr[0])|uint16(p[0])<<8))
		p = p[1:]
		w.hasCarry = false
	}
	usable := len(p) &^ 1
	for i := 0; i < usable; i += 2 {
		w.inSamp = append(w.inSamp, int16(uint16(p[i])|uint16(p[i+1])<<8))
	}
	if usable < len(p) {
		w.carryArr[0] = p[usable]
		w.hasCarry = true
	}
	if len(w.inSamp) == 0 {
		return total, nil
	}

	in := w.inSamp
	if w.c.inRS != nil {
		w.rsIn = w.c.inRS.ResampleInto(w.rsIn, in)
		in = w.rsIn
	}
	if need := w.accN + len(in); need > len(w.acc) {
		grown := make([]int16, need*2)
		copy(grown, w.acc[:w.accN])
		w.acc = grown
	}
	copy(w.acc[w.accN:], in)
	w.accN += len(in)

	w.out = w.out[:0]
	hop := w.c.hop
	for w.accN >= hop {
		copy(w.block, w.acc[:hop])
		w.c.process(w.block)
		outSamples := w.block
		if w.c.outRS != nil {
			w.rsOut = w.c.outRS.ResampleInto(w.rsOut, w.block)
			outSamples = w.rsOut
		}
		for _, v := range outSamples {
			w.out = binary.LittleEndian.AppendUint16(w.out, uint16(v))
		}
		copy(w.acc, w.acc[hop:w.accN])
		w.accN -= hop
	}
	if len(w.out) > 0 {
		if _, err := w.dst.Write(w.out); err != nil {
			return 0, err
		}
	}
	return total, nil
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }
