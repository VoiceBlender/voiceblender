package audiofilter

import (
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/VoiceBlender/voiceblender/internal/mixer"
)

// defaultBlockMs is the block length used when no filter demands a specific
// frame size.
const defaultBlockMs = 20

// fadeMs is the ramp each way around a chain change. Long enough that the swap
// is a soft dip rather than a click, short enough not to read as a dropout.
const fadeMs = 8

// ResolveWorkRate reports the rate a chain will run at: the highest any filter
// demands, or dstRate when none of them care. A chain of rate-agnostic filters
// therefore adds no resampling beyond the leg/room conversion already required.
func ResolveWorkRate(specs []Spec, dstRate int) int {
	work := 0
	for _, s := range specs {
		if d, ok := lookup(s.Type); ok && d.RequiredRate > work {
			work = d.RequiredRate
		}
	}
	if work == 0 {
		return dstRate
	}
	return work
}

func lcm(a, b int) int {
	if a == 0 {
		return b
	}
	if b == 0 {
		return a
	}
	x, y := a, b
	for y != 0 {
		x, y = y, x%y
	}
	return a / x * b
}

// chain holds the built stages plus the rate and framing they run under.
type chain struct {
	stages []Stage
	frames []int // per-stage frame length; 0 means "the whole block"
	work   int
	hop    int
	inRS   *mixer.PCMResampler
	outRS  *mixer.PCMResampler
}

func buildChain(srcRate, dstRate int, specs []Spec) (*chain, error) {
	if err := Validate(specs); err != nil {
		return nil, err
	}
	work := ResolveWorkRate(specs, dstRate)
	c := &chain{work: work}
	for _, s := range specs {
		d, ok := lookup(s.Type)
		if !ok {
			return nil, fmt.Errorf("unknown filter %q", s.Type)
		}
		stage, err := d.New(work, s.Params)
		if err != nil {
			c.Close()
			return nil, fmt.Errorf("filter %q: %w", strings.ToLower(s.Type), err)
		}
		c.stages = append(c.stages, stage)
		c.frames = append(c.frames, d.FrameSamples)
		c.hop = lcm(c.hop, d.FrameSamples)
	}
	if c.hop == 0 {
		c.hop = work / 1000 * defaultBlockMs
	}
	c.inRS = mixer.NewPCMResampler(srcRate, work)
	c.outRS = mixer.NewPCMResampler(work, dstRate)
	return c, nil
}

func (c *chain) process(block []int16) {
	for i, s := range c.stages {
		n := c.frames[i]
		if n <= 0 || n >= len(block) {
			s.Process(block)
			continue
		}
		for off := 0; off+n <= len(block); off += n {
			s.Process(block[off : off+n])
		}
	}
}

func (c *chain) Close() error {
	var first error
	for _, s := range c.stages {
		if err := s.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Build wraps src, which yields mono s16 PCM at srcRate, and produces filtered
// PCM at dstRate. An empty chain is still a Reader rather than the plain
// resampling reader: the chain can then be changed mid-call, and it costs
// nothing to do so — an empty chain benchmarks identical to the plain reader
// and allocates less, because this path is allocation-free and that one is not.
func Build(src io.Reader, srcRate, dstRate int, specs []Spec) (*Reader, error) {
	c, err := buildChain(srcRate, dstRate, specs)
	if err != nil {
		return nil, err
	}
	srcSamples := c.hop*srcRate/c.work + 8
	return &Reader{
		src:     src,
		c:       c,
		srcRate: srcRate,
		dstRate: dstRate,
		specs:   append([]Spec(nil), specs...),
		srcBuf:  make([]byte, srcSamples*2),
		inSamp:  make([]int16, 0, srcSamples),
		rsIn:    make([]int16, 0, c.hop+64),
		acc:     make([]int16, c.hop*2),
		block:   make([]int16, c.hop),
		rsOut:   make([]int16, 0, c.hop*dstRate/c.work+64),
		out:     make([]byte, 0, c.hop*4),
		fade:    1,
	}, nil
}

// Reader decouples three framings that do not line up: the source delivers
// arbitrary byte counts, stages need whole blocks at the working rate, and the
// consumer reads arbitrary sizes. Every buffer here is allocated once and
// reused, because this runs per frame on every filtered leg.
//
// The chain can be replaced while audio is flowing; see SetFilters.
type Reader struct {
	// mu guards the chain and every buffer it touches. It is never held across
	// the source read, so a change cannot block behind a silent leg.
	mu      sync.Mutex
	srcRate int
	dstRate int
	specs   []Spec

	src io.Reader
	c   *chain

	// A chain change is staged rather than applied immediately, and handed
	// over inside the read loop between blocks. A rate change rebuilds the
	// resamplers and resizes the block, so the swap is wrapped in a short fade
	// — otherwise it lands as a click mid-sentence.
	// observer receives a copy of the filtered output. Voice activity detection
	// hangs off this: run upstream of the chain it would score the noise the
	// chain exists to remove. The writer must not block — the audio path is on
	// a real-time budget.
	observer io.Writer

	pending      *chain
	pendingSpecs []Spec
	fade         float64 // output gain, 0..1
	fadeStep     float64 // per-sample ramp
	fadingOut    bool

	srcBuf   []byte
	carryArr [1]byte
	hasCarry bool

	inSamp []int16
	rsIn   []int16
	acc    []int16
	accN   int
	block  []int16
	rsOut  []int16

	out    []byte
	outOff int

	done bool
	err  error
}

// Close releases the stages, returning whatever they hold — a denoise stage
// hands its kernel state back to the pool here. The mixer calls it when a
// participant is torn down, which includes a leg moving between rooms.
//
// It deliberately does NOT close the source. The chain does not own it: the leg
// does, and the same reader is handed to the next room on a move. Closing it
// would leave a pipe-backed leg (a LiveKit participant's AudioReader is an
// io.PipeReader) permanently silent after the move. Before this type existed
// the participant reader was a *resampleReader, which has no Close at all, so
// the mixer never closed a leg's source — that behaviour is preserved.
func (r *Reader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending != nil {
		r.pending.Close()
		r.pending, r.pendingSpecs = nil, nil
	}
	return r.c.Close()
}

func (r *Reader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		r.mu.Lock()
		if r.outOff < len(r.out) {
			n := copy(p, r.out[r.outOff:])
			r.outOff += n
			r.mu.Unlock()
			return n, nil
		}
		if r.done {
			err := r.err
			r.mu.Unlock()
			if err != nil {
				return 0, err
			}
			return 0, io.EOF
		}
		r.out, r.outOff = r.out[:0], 0
		r.mu.Unlock()

		// Unlocked: src.Read blocks until the leg sends audio, and holding the
		// lock across it would stall SetFilters on a silent call. srcBuf is
		// touched only by this goroutine, so it needs no guard.
		n, err := r.src.Read(r.srcBuf)

		r.mu.Lock()
		if n > 0 {
			r.feed(r.srcBuf[:n])
		}
		if err != nil {
			r.err = err
			r.done = true
			r.flush()
		}
		r.mu.Unlock()
	}
}

// SetObserver attaches a writer that receives a copy of the filtered output,
// or nil to detach. Safe to call while audio is flowing.
func (r *Reader) SetObserver(w io.Writer) {
	r.mu.Lock()
	r.observer = w
	r.mu.Unlock()
}

// Filters reports the chain currently running.
func (r *Reader) Filters() []Spec {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Test the chain, not the spec slice: a staged *empty* chain has nil specs
	// and would otherwise be read as "nothing staged".
	if r.pending != nil {
		return append([]Spec(nil), r.pendingSpecs...)
	}
	return append([]Spec(nil), r.specs...)
}

// SetFilters replaces the chain on a live stream. The old stages are closed,
// releasing whatever they hold — a denoise stage returns its kernel state to
// the pool here.
//
// The working rate must not change, because the resamplers and block size are
// sized for it: swapping `robotic` for `pitch` is fine, but adding or removing
// `denoise` moves the chain to 48 kHz and needs a rebuild rather than a swap.
// That is reported rather than done silently, since a rebuild drops the
// accumulated samples and would click.
func (r *Reader) SetFilters(specs []Spec) error {
	if err := Validate(specs); err != nil {
		return err
	}
	next, err := buildChain(r.srcRate, r.dstRate, specs)
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// A same-rate change can keep the running resamplers: they hold filter
	// history, and fresh ones would emit their zero history as a gap. A rate
	// change cannot — the new chain's resamplers are sized differently.
	if next.work == r.c.work && next.hop == r.c.hop {
		next.inRS, next.outRS = r.c.inRS, r.c.outRS
	}
	if r.pending != nil {
		// Superseded before it was ever applied.
		r.pending.Close()
	}
	r.pending, r.pendingSpecs = next, append([]Spec(nil), specs...)
	r.fadingOut = true
	r.fadeStep = 1 / float64(r.dstRate*fadeMs/1000)
	return nil
}

// applyFade ramps the block's output and performs a staged chain change when
// the fade reaches silence. Running the swap here, between blocks on the read
// goroutine, keeps it off the caller's thread and away from a half-processed
// block.
func (r *Reader) applyFade(out []int16) []int16 {
	if r.pending == nil && r.fade >= 1 {
		return out
	}
	for i, v := range out {
		out[i] = int16(float64(v) * r.fade)
		if r.fadingOut {
			r.fade -= r.fadeStep
			if r.fade <= 0 {
				r.fade = 0
				r.swapPending()
			}
		} else if r.fade < 1 {
			r.fade += r.fadeStep
			if r.fade > 1 {
				r.fade = 1
			}
		}
	}
	return out
}

// swapPending installs the staged chain. Anything still in the accumulator
// belongs to the old working rate and is dropped — at most one block, and the
// output is at silence by this point, so it is inaudible.
func (r *Reader) swapPending() {
	if r.pending == nil {
		r.fadingOut = false
		return
	}
	old := r.c
	r.c = r.pending
	r.specs = r.pendingSpecs
	r.pending, r.pendingSpecs = nil, nil
	r.fadingOut = false

	r.accN = 0
	if cap(r.acc) < r.c.hop*2 {
		r.acc = make([]int16, r.c.hop*2)
	} else {
		r.acc = r.acc[:r.c.hop*2]
	}
	if cap(r.block) < r.c.hop {
		r.block = make([]int16, r.c.hop)
	} else {
		r.block = r.block[:r.c.hop]
	}
	r.rsIn, r.rsOut = r.rsIn[:0], r.rsOut[:0]
	if want := (r.c.hop*r.srcRate/r.c.work + 8) * 2; cap(r.srcBuf) < want {
		r.srcBuf = make([]byte, want)
	} else {
		r.srcBuf = r.srcBuf[:want]
	}

	// The old chain keeps its resamplers only when they were not handed over.
	if old.inRS == r.c.inRS {
		old.inRS, old.outRS = nil, nil
	}
	old.Close()
}

// feed decodes source bytes, resamples them to the working rate and runs every
// block they complete. A source Read may split a sample across calls, so the
// odd trailing byte is carried rather than dropped.
func (r *Reader) feed(b []byte) {
	r.inSamp = r.inSamp[:0]
	if r.hasCarry && len(b) > 0 {
		r.inSamp = append(r.inSamp, int16(uint16(r.carryArr[0])|uint16(b[0])<<8))
		b = b[1:]
		r.hasCarry = false
	}
	usable := len(b) &^ 1
	for i := 0; i < usable; i += 2 {
		r.inSamp = append(r.inSamp, int16(uint16(b[i])|uint16(b[i+1])<<8))
	}
	if usable < len(b) {
		r.carryArr[0] = b[usable]
		r.hasCarry = true
	}
	if len(r.inSamp) == 0 {
		return
	}
	r.accumulate(r.resampleIn(r.inSamp))
	r.drain()
}

func (r *Reader) resampleIn(in []int16) []int16 {
	if r.c.inRS == nil {
		return in
	}
	r.rsIn = r.c.inRS.ResampleInto(r.rsIn, in)
	return r.rsIn
}

func (r *Reader) accumulate(s []int16) {
	if need := r.accN + len(s); need > len(r.acc) {
		grown := make([]int16, need*2)
		copy(grown, r.acc[:r.accN])
		r.acc = grown
	}
	copy(r.acc[r.accN:], s)
	r.accN += len(s)
}

func (r *Reader) drain() {
	for {
		// Re-read the chain each pass: emit can install a staged chain, which
		// changes the block size and clears the accumulator. Caching either
		// across the swap slices a buffer that no longer exists.
		c := r.c
		hop := c.hop
		if r.accN < hop {
			return
		}
		copy(r.block, r.acc[:hop])
		c.process(r.block)
		r.emit(r.block)
		if r.c != c {
			continue // swapped; the accumulator was reset with it
		}
		copy(r.acc, r.acc[hop:r.accN])
		r.accN -= hop
	}
}

// flush zero-pads and emits a trailing partial block at EOF, keeping only the
// portion that corresponds to real input.
func (r *Reader) flush() {
	if r.accN == 0 {
		return
	}
	keep := r.accN
	copy(r.block, r.acc[:keep])
	for i := keep; i < len(r.block); i++ {
		r.block[i] = 0
	}
	r.c.process(r.block)
	out := r.resampleOut(r.block)
	lim := len(out) * keep / r.c.hop
	if lim > len(out) {
		lim = len(out)
	}
	r.appendOut(r.applyFade(out[:lim]))
	r.accN = 0
}

func (r *Reader) resampleOut(block []int16) []int16 {
	if r.c.outRS == nil {
		return block
	}
	r.rsOut = r.c.outRS.ResampleInto(r.rsOut, block)
	return r.rsOut
}

func (r *Reader) emit(block []int16) { r.appendOut(r.applyFade(r.resampleOut(block))) }

func (r *Reader) appendOut(s []int16) {
	start := len(r.out)
	for _, v := range s {
		r.out = binary.LittleEndian.AppendUint16(r.out, uint16(v))
	}
	if r.observer != nil {
		r.observer.Write(r.out[start:])
	}
}
