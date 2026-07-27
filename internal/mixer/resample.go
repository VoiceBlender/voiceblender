package mixer

import (
	"encoding/binary"
	"io"
	"math"

	"github.com/VoiceBlender/voiceblender/internal/resampler"
)

// resamplerQuality selects the polyphase filter length: it is a latency budget
// (group delay scales with it) as much as a quality knob. 4 is the lowest
// quality that holds 3.4 kHz flat at 16k<->8k; higher values only extend the
// response above the telephony band while adding delay and CPU. See
// TestResample_Passband, TestResample_GroupDelay, and BenchmarkPCMResampler.
const resamplerQuality = 4

// resampleSlack is output-buffer headroom, in samples: ProcessFloat64 silently
// drops input if its output buffer fills, and the per-chunk output count varies
// around the nominal ratio as the filter's fractional phase advances.
const resampleSlack = 128

// PCMResampler converts mono 16-bit PCM between two sample rates through an
// anti-aliased polyphase filter, so energy above the destination Nyquist is
// removed instead of folding back into the passband.
//
// It is stateful: the filter carries history across calls to keep chunk
// boundaries continuous, so one instance must be built per stream (or per
// destination rate, for a stream whose destination can change) and retained for
// that stream's lifetime. Building one per frame re-zeroes the filter and emits
// that zero history as every frame's leading samples — worse than no filtering.
// Not safe for concurrent use.
//
// Filtering adds a constant group delay (OutputLatency reports it; ~4 ms at
// 8k<->16k, quality 4) but does not change sample counts: N ms in, N ms out.
//
// NewPCMResampler returns nil when the rates match; a nil *PCMResampler is a
// valid passthrough, so callers need no equal-rate special case.
type PCMResampler struct {
	srcRate int
	dstRate int
	rs      *resampler.Resampler
	in      []float64 // scratch, reused across calls
	out     []float64 // scratch, reused across calls
}

// NewPCMResampler returns a resampler from srcRate to dstRate, or nil when the
// rates are equal (passthrough — see PCMResampler).
func NewPCMResampler(srcRate, dstRate int) *PCMResampler {
	return newPCMResampler(srcRate, dstRate, resamplerQuality)
}

// newPCMResampler is NewPCMResampler with quality left open, so the benchmark
// can sweep quality against cost, attenuation, and group delay.
func newPCMResampler(srcRate, dstRate, quality int) *PCMResampler {
	if srcRate == dstRate {
		return nil
	}
	return &PCMResampler{
		srcRate: srcRate,
		dstRate: dstRate,
		rs:      resampler.New(1, srcRate, dstRate, quality),
	}
}

// OutputLatency reports the filter's group delay in destination samples.
func (r *PCMResampler) OutputLatency() int {
	if r == nil {
		return 0
	}
	return r.rs.OutputLatency()
}

// ResampleSamples converts in to the destination rate, returning a new slice.
// A nil receiver returns in unchanged.
func (r *PCMResampler) ResampleSamples(in []int16) []int16 {
	if r == nil || len(in) == 0 {
		return in
	}
	fin := r.scratchIn(len(in))
	for i, s := range in {
		fin[i] = float64(s) / 32768.0
	}
	fout := r.process(fin)
	out := make([]int16, len(fout))
	for i, s := range fout {
		out[i] = clampToInt16(s)
	}
	return out
}

// ResampleBytes converts a mono 16-bit little-endian PCM buffer to the
// destination rate, returning a new buffer. The input must hold a whole number
// of samples; a trailing odd byte is dropped. A nil receiver returns p
// unchanged.
func (r *PCMResampler) ResampleBytes(p []byte) []byte {
	if r == nil || len(p) < 2 {
		return p
	}
	fin := r.scratchIn(len(p) / 2)
	for i := range fin {
		fin[i] = float64(int16(binary.LittleEndian.Uint16(p[i*2:]))) / 32768.0
	}
	fout := r.process(fin)
	out := make([]byte, len(fout)*2)
	for i, s := range fout {
		binary.LittleEndian.PutUint16(out[i*2:], uint16(clampToInt16(s)))
	}
	return out
}

func (r *PCMResampler) scratchIn(n int) []float64 {
	if cap(r.in) < n {
		r.in = make([]float64, n)
	}
	return r.in[:n]
}

// process filters in and returns the samples the resampler wrote. The result is
// backed by the receiver's scratch buffer and is only valid until the next call.
func (r *PCMResampler) process(in []float64) []float64 {
	need := len(in)*r.dstRate/r.srcRate + resampleSlack
	if cap(r.out) < need {
		r.out = make([]float64, need)
	}
	out := r.out[:need]
	_, written := r.rs.ProcessFloat64(0, in, out)
	return out[:written]
}

// clampToInt16 denormalizes a [-1,1] sample to int16. The clamp is mandatory:
// filter overshoot can push a full-scale sample past int16 range, which would
// otherwise wrap into loud noise.
func clampToInt16(s float64) int16 {
	return int16(math.Max(math.Min(s*32768.0, 32767), -32768))
}

// NewResampleReader wraps src to produce PCM at dstRate from srcRate input.
// Returns src unchanged when rates match (passthrough).
func NewResampleReader(src io.Reader, srcRate, dstRate int) io.Reader {
	if srcRate == dstRate {
		return src
	}
	return &resampleReader{
		src:     src,
		srcRate: srcRate,
		dstRate: dstRate,
		rs:      NewPCMResampler(srcRate, dstRate),
	}
}

// NewResampleWriter wraps dst to accept PCM at inputRate and write at outputRate.
// Returns dst unchanged when rates match (passthrough).
func NewResampleWriter(dst io.Writer, inputRate, outputRate int) io.Writer {
	if inputRate == outputRate {
		return dst
	}
	return &resampleWriter{
		dst:        dst,
		inputRate:  inputRate,
		outputRate: outputRate,
		rs:         NewPCMResampler(inputRate, outputRate),
	}
}

// resampleReader converts PCM from one sample rate to another. The resampler is
// built once here and retained, so filter history carries across every Read and
// chunk boundaries stay continuous.
type resampleReader struct {
	src     io.Reader
	srcRate int
	dstRate int
	rs      *PCMResampler
	buf     []byte // leftover output bytes not yet consumed
}

func (r *resampleReader) Read(p []byte) (int, error) {
	// Serve from leftover buffer first
	if len(r.buf) > 0 {
		n := copy(p, r.buf)
		r.buf = r.buf[n:]
		return n, nil
	}

	// Calculate how many source bytes to read based on the rate ratio.
	// We want to produce len(p) output bytes, so we need approximately
	// len(p) * srcRate / dstRate input bytes. Read in multiples of 2 (one sample).
	srcSize := len(p) * r.srcRate / r.dstRate
	if srcSize < 2 {
		srcSize = 2
	}
	srcSize = (srcSize / 2) * 2

	srcBuf := make([]byte, srcSize)
	n, err := r.src.Read(srcBuf)
	if n < 2 {
		if err != nil {
			return 0, err
		}
		return 0, nil
	}
	n = (n / 2) * 2

	out := r.rs.ResampleBytes(srcBuf[:n])

	copied := copy(p, out)
	if copied < len(out) {
		r.buf = out[copied:]
	}
	return copied, err
}

// resampleWriter converts incoming PCM at inputRate to outputRate before
// writing. The resampler is built once in NewResampleWriter and retained, so
// filter history carries across every Write.
type resampleWriter struct {
	dst        io.Writer
	inputRate  int
	outputRate int
	rs         *PCMResampler
	buf        []byte // accumulate partial samples
}

// Close closes the wrapped writer, so that closing a rate-crossing leg's
// writer reaches the leg's egress rather than stopping at the resampler.
// Without it every 8 kHz leg is unclosable and the mixer's panic path cannot
// wake its owner. Buffered partial samples are dropped: the frame they would
// complete is worthless on a writer that is being torn down.
func (w *resampleWriter) Close() error {
	return closeWriter(w.dst)
}

func (w *resampleWriter) Write(p []byte) (int, error) {
	total := len(p)

	data := p
	if len(w.buf) > 0 {
		data = append(w.buf, p...)
		w.buf = nil
	}

	// Need at least 2 bytes (one sample)
	usable := (len(data) / 2) * 2
	if usable < 2 {
		w.buf = append(w.buf, data...)
		return total, nil
	}

	remainder := data[usable:]
	out := w.rs.ResampleBytes(data[:usable])

	if len(remainder) > 0 {
		w.buf = append(w.buf, remainder...)
	}

	if len(out) == 0 {
		return total, nil
	}
	if _, err := w.dst.Write(out); err != nil {
		return 0, err
	}
	return total, nil
}
