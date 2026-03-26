package codec

// G.722 codec implementation based on the ITU-T G.722 specification.
// Reference: spandsp by Steve Underwood (LGPL 2.1)
// Original: CMU 1993, Computer Science Speech Group
//
// Ported from github.com/GetStream chat/ingress/sip/audio/codec.

import (
	"encoding/binary"
	"io"
)

// RTPBufSize is the maximum RTP packet buffer size.
const RTPBufSize = 1500

// ---------------------------------------------------------------------------
// QMF filter coefficients (12-tap, split into forward and reverse)
// ---------------------------------------------------------------------------

var (
	g722QMFCoeffsFwd = [12]int{3, -11, 12, 32, -210, 951, 3876, -805, 362, -156, 53, -11}
	g722QMFCoeffsRev = [12]int{-11, 53, -156, 362, -805, 3876, 951, -210, 32, 12, -11, 3}
)

// ---------------------------------------------------------------------------
// G.722 lookup tables
// ---------------------------------------------------------------------------

var (
	// Lower sub-band inverse quantizer (2-bit, for higher sub-band)
	g722QM2 = [4]int{-7408, -1616, 7408, 1616}

	// Lower sub-band inverse quantizer (4-bit, for adaptation loop)
	g722QM4 = [16]int{
		0, -20456, -12896, -8968,
		-6288, -4240, -2584, -1200,
		20456, 12896, 8968, 6288,
		4240, 2584, 1200, 0,
	}

	// Lower sub-band inverse quantizer (6-bit, for output reconstruction in 64kbps mode)
	g722QM6 = [64]int{
		-136, -136, -136, -136,
		-24808, -21904, -19008, -16704,
		-14984, -13512, -12280, -11192,
		-10232, -9360, -8576, -7856,
		-7192, -6576, -6000, -5456,
		-4944, -4464, -4008, -3576,
		-3168, -2776, -2400, -2032,
		-1688, -1360, -1040, -728,
		24808, 21904, 19008, 16704,
		14984, 13512, 12280, 11192,
		10232, 9360, 8576, 7856,
		7192, 6576, 6000, 5456,
		4944, 4464, 4008, 3576,
		3168, 2776, 2400, 2032,
		1688, 1360, 1040, 728,
		432, 136, -432, -136,
	}

	// Encoder quantizer decision thresholds
	g722Q6 = [32]int{
		0, 35, 72, 110, 150, 190, 233, 276,
		323, 370, 422, 473, 530, 587, 650, 714,
		786, 858, 940, 1023, 1121, 1219, 1339, 1458,
		1612, 1765, 1980, 2195, 2557, 2919, 0, 0,
	}

	// Encoder quantizer output mapping for negative input
	g722ILN = [32]int{
		0, 63, 62, 31, 30, 29, 28, 27,
		26, 25, 24, 23, 22, 21, 20, 19,
		18, 17, 16, 15, 14, 13, 12, 11,
		10, 9, 8, 7, 6, 5, 4, 0,
	}

	// Encoder quantizer output mapping for positive input
	g722ILP = [32]int{
		0, 61, 60, 59, 58, 57, 56, 55,
		54, 53, 52, 51, 50, 49, 48, 47,
		46, 45, 44, 43, 42, 41, 40, 39,
		38, 37, 36, 35, 34, 33, 32, 0,
	}

	// High band encoder output mapping for negative input
	g722IHN = [3]int{0, 1, 0}

	// High band encoder output mapping for positive input
	g722IHP = [3]int{0, 3, 2}

	// Inverse log table for scale factor computation
	g722ILB = [32]int{
		2048, 2093, 2139, 2186, 2233, 2282, 2332, 2383,
		2435, 2489, 2543, 2599, 2656, 2714, 2774, 2834,
		2896, 2960, 3025, 3091, 3158, 3228, 3298, 3371,
		3444, 3520, 3597, 3676, 3756, 3838, 3922, 4008,
	}

	// Lower sub-band log scale adaptation weights
	g722WL = [8]int{-60, -30, 58, 172, 334, 538, 1198, 3042}

	// Lower sub-band LOGSCL adaptation index
	g722RL42 = [16]int{0, 7, 6, 5, 4, 3, 2, 1, 7, 6, 5, 4, 3, 2, 1, 0}

	// Higher sub-band log scale adaptation weights
	g722WH = [3]int{0, -214, 798}

	// Higher sub-band LOGSCH adaptation index
	g722RH2 = [4]int{2, 1, 2, 1}
)

// ---------------------------------------------------------------------------
// Per-band adaptive predictor state
// ---------------------------------------------------------------------------

// g722BandState holds the per-band encoder/decoder state.
type g722BandState struct {
	nb  int
	det int
	s   int    // Predicted signal (output of PREDIC)
	sz  int    // Zero-section output (FILTEZ)
	r   int    // Previous reconstructed signal
	p   [2]int // Partial reconstruction history
	a   [2]int // Pole predictor coefficients
	b   [6]int // Zero predictor coefficients
	d   [7]int // Difference signal history
}

// ---------------------------------------------------------------------------
// Helper functions
// ---------------------------------------------------------------------------

func g722Saturate16(val int) int {
	if val > 32767 {
		return 32767
	}
	if val < -32768 {
		return -32768
	}
	return val
}

func g722Saturate(val int) int16 {
	if val > 32767 {
		return 32767
	}
	if val < -32768 {
		return -32768
	}
	return int16(val)
}

func g722Saturate15(val int) int {
	if val > 16383 {
		return 16383
	}
	if val < -16384 {
		return -16384
	}
	return val
}

func g722IntAbs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// block4 performs the adaptive predictor update.
// Implements RECONS, PARREC, UPPOL2, UPPOL1, FILTEP, UPZERO, FILTEZ, PREDIC.
func g722Block4(s *g722BandState, dx int) {
	// RECONS
	r := g722Saturate16(s.s + dx)
	// PARREC
	p := g722Saturate16(s.sz + dx)

	// UPPOL2 - Update second pole coefficient
	wd1 := g722Saturate16(s.a[0] << 2)
	wd32 := -wd1
	if (p^s.p[0])&0x8000 != 0 {
		wd32 = wd1
	}
	if wd32 > 32767 {
		wd32 = 32767
	}
	signBit := 128
	if (p^s.p[1])&0x8000 != 0 {
		signBit = -128
	}
	wd3 := signBit + (wd32 >> 7) + ((s.a[1] * 32512) >> 15)
	if g722IntAbs(wd3) > 12288 {
		if wd3 < 0 {
			wd3 = -12288
		} else {
			wd3 = 12288
		}
	}
	ap1 := wd3

	// UPPOL1 - Update first pole coefficient
	wd1i := -192
	if (p^s.p[0])&0x8000 == 0 {
		wd1i = 192
	}
	wd2i := (s.a[0] * 32640) >> 15
	ap0 := g722Saturate16(wd1i + wd2i)
	limit := g722Saturate16(15360 - ap1)
	if g722IntAbs(ap0) > limit {
		if ap0 < 0 {
			ap0 = -limit
		} else {
			ap0 = limit
		}
	}

	// FILTEP - Pole section filter
	r2 := g722Saturate16(r + r)
	filtepWd1 := (ap0 * r2) >> 15
	prevR2 := g722Saturate16(s.r + s.r)
	filtepWd2 := (ap1 * prevR2) >> 15
	sp := g722Saturate16(filtepWd1 + filtepWd2)

	// Update pole state
	s.r = r
	s.a[0] = ap0
	s.a[1] = ap1
	s.p[1] = s.p[0]
	s.p[0] = p

	// UPZERO / DELAYA / FILTEZ
	upzeroWd1 := 0
	if dx != 0 {
		upzeroWd1 = 128
	}
	s.d[0] = dx
	sz := 0
	for i := 5; i >= 0; i-- {
		wd2u := upzeroWd1
		if (s.d[i+1]^dx)&0x8000 != 0 {
			wd2u = -upzeroWd1
		}
		wd3u := (s.b[i] * 32640) >> 15
		s.b[i] = g722Saturate16(wd2u + wd3u)
		d2 := g722Saturate16(s.d[i] + s.d[i])
		sz += (s.b[i] * d2) >> 15
		s.d[i+1] = s.d[i]
	}
	s.sz = g722Saturate16(sz)

	// PREDIC
	s.s = g722Saturate16(sp + s.sz)
}

// circularDot computes dot product with circular buffer starting at ptr.
func g722CircularDot(buf *[12]int, coeffs *[12]int, ptr int) int {
	sum := 0
	for i := 0; i < 12; i++ {
		idx := (ptr + i) % 12
		sum += buf[idx] * coeffs[i]
	}
	return sum
}

// ---------------------------------------------------------------------------
// G722Decoder
// ---------------------------------------------------------------------------

// G722Decoder decodes G.722 encoded data to 16kHz 16-bit PCM samples.
type G722Decoder struct {
	bandLow  g722BandState
	bandHigh g722BandState
	qmfX     [12]int // rlow + rhigh history
	qmfY     [12]int // rlow - rhigh history
	qmfPtr   int
}

// NewG722Decoder creates a new G.722 decoder with initial state.
func NewG722Decoder() *G722Decoder {
	d := &G722Decoder{}
	d.Reset()
	return d
}

// Decode decodes G.722 data to 16kHz PCM samples.
// Each input byte produces 2 output samples.
func (d *G722Decoder) Decode(data []byte) ([]int16, error) {
	samples := make([]int16, len(data)*2)

	for i, code := range data {
		ilow := int(code & 0x3F)
		ihigh := int(code>>6) & 0x03

		// Block 5L, LOW BAND INVQBL - Full 6-bit reconstruction for output
		wd2 := g722QM6[ilow]
		wd2 = (d.bandLow.det * wd2) >> 15
		rlow := g722Saturate15(d.bandLow.s + wd2)

		// Block 2L, INVQAL - 4-bit reconstruction for state update
		ril := ilow >> 2
		dlow := (d.bandLow.det * g722QM4[ril]) >> 15

		// Block 3L, LOGSCL
		il4 := g722RL42[ril]
		wd1 := (d.bandLow.nb * 127) >> 7
		wd1 += g722WL[il4]
		if wd1 < 0 {
			wd1 = 0
		} else if wd1 > 18432 {
			wd1 = 18432
		}
		d.bandLow.nb = wd1

		// Block 3L, SCALEL
		wd1 = (d.bandLow.nb >> 6) & 31
		wd2s := 8 - (d.bandLow.nb >> 11)
		var wd3 int
		if wd2s < 0 {
			wd3 = g722ILB[wd1] << (-wd2s)
		} else {
			wd3 = g722ILB[wd1] >> wd2s
		}
		d.bandLow.det = wd3 << 2

		g722Block4(&d.bandLow, dlow)

		// Block 2H, INVQAH
		dhigh := (d.bandHigh.det * g722QM2[ihigh]) >> 15
		rhigh := g722Saturate15(dhigh + d.bandHigh.s)

		// Block 3H, LOGSCH
		ih2 := g722RH2[ihigh]
		wd1 = (d.bandHigh.nb * 127) >> 7
		wd1 += g722WH[ih2]
		if wd1 < 0 {
			wd1 = 0
		} else if wd1 > 22528 {
			wd1 = 22528
		}
		d.bandHigh.nb = wd1

		// Block 3H, SCALEH
		wd1 = (d.bandHigh.nb >> 6) & 31
		wd2s = 10 - (d.bandHigh.nb >> 11)
		if wd2s < 0 {
			wd3 = g722ILB[wd1] << (-wd2s)
		} else {
			wd3 = g722ILB[wd1] >> wd2s
		}
		d.bandHigh.det = wd3 << 2

		g722Block4(&d.bandHigh, dhigh)

		// QMF synthesis filter - dual 12-element circular buffers
		d.qmfX[d.qmfPtr] = rlow + rhigh
		d.qmfY[d.qmfPtr] = rlow - rhigh
		d.qmfPtr++
		if d.qmfPtr >= 12 {
			d.qmfPtr = 0
		}

		// Shift by 12 for QMF DC gain (4096), less 1 for 15-bit G.722 internal signals
		samples[i*2] = g722Saturate(g722CircularDot(&d.qmfY, &g722QMFCoeffsRev, d.qmfPtr) >> 11)
		samples[i*2+1] = g722Saturate(g722CircularDot(&d.qmfX, &g722QMFCoeffsFwd, d.qmfPtr) >> 11)
	}

	return samples, nil
}

// Reset resets the decoder to its initial state.
func (d *G722Decoder) Reset() {
	d.bandLow = g722BandState{det: 32}
	d.bandHigh = g722BandState{det: 8}
	d.qmfX = [12]int{}
	d.qmfY = [12]int{}
	d.qmfPtr = 0
}

// ---------------------------------------------------------------------------
// G722Encoder
// ---------------------------------------------------------------------------

// G722Encoder encodes 16kHz 16-bit PCM samples to G.722.
type G722Encoder struct {
	bandLow  g722BandState
	bandHigh g722BandState
	qmfX     [12]int // even samples
	qmfY     [12]int // odd samples
	qmfPtr   int
}

// NewG722Encoder creates a new G.722 encoder with initial state.
func NewG722Encoder() *G722Encoder {
	e := &G722Encoder{}
	e.Reset()
	return e
}

// Encode encodes 16kHz PCM samples to G.722 data.
// Input samples must be in pairs (2 samples per encoded byte).
func (e *G722Encoder) Encode(samples []int16) ([]byte, error) {
	numPairs := len(samples) / 2
	data := make([]byte, numPairs)

	for i := 0; i < numPairs*2; i += 2 {
		// QMF analysis filter
		xin0 := int(samples[i])
		if xin0 > 16350 {
			xin0 = 16350
		} else if xin0 < -16350 {
			xin0 = -16350
		}
		e.qmfX[e.qmfPtr] = xin0

		xin1 := int(samples[i+1])
		if xin1 > 16350 {
			xin1 = 16350
		} else if xin1 < -16350 {
			xin1 = -16350
		}
		e.qmfY[e.qmfPtr] = xin1

		e.qmfPtr++
		if e.qmfPtr >= 12 {
			e.qmfPtr = 0
		}

		sumodd := g722CircularDot(&e.qmfX, &g722QMFCoeffsFwd, e.qmfPtr)
		sumeven := g722CircularDot(&e.qmfY, &g722QMFCoeffsRev, e.qmfPtr)

		// Shift by 14: 12 for QMF gain + 1 for summing two filters + 1 for 15-bit G.722 input
		xlow := (sumeven + sumodd) >> 14
		xhigh := (sumeven - sumodd) >> 14

		// Block 1L, SUBTRA
		el := g722Saturate16(xlow - e.bandLow.s)

		// Block 1L, QUANTL
		wd := el
		if wd < 0 {
			wd = -(wd + 1)
		}

		iIdx := 1
		for iIdx < 30 {
			wd1 := (g722Q6[iIdx] * e.bandLow.det) >> 12
			if wd < wd1 {
				break
			}
			iIdx++
		}

		var ilow int
		if el < 0 {
			ilow = g722ILN[iIdx]
		} else {
			ilow = g722ILP[iIdx]
		}

		// Block 2L, INVQAL
		ril := ilow >> 2
		dlow := (e.bandLow.det * g722QM4[ril]) >> 15

		// Block 3L, LOGSCL
		il4 := g722RL42[ril]
		wd2 := (e.bandLow.nb * 127) >> 7
		e.bandLow.nb = wd2 + g722WL[il4]
		if e.bandLow.nb < 0 {
			e.bandLow.nb = 0
		} else if e.bandLow.nb > 18432 {
			e.bandLow.nb = 18432
		}

		// Block 3L, SCALEL
		wd1 := (e.bandLow.nb >> 6) & 31
		wd2 = 8 - (e.bandLow.nb >> 11)
		var wd3 int
		if wd2 < 0 {
			wd3 = g722ILB[wd1] << (-wd2)
		} else {
			wd3 = g722ILB[wd1] >> wd2
		}
		e.bandLow.det = wd3 << 2

		g722Block4(&e.bandLow, dlow)

		// Block 1H, SUBTRA
		eh := g722Saturate16(xhigh - e.bandHigh.s)

		// Block 1H, QUANTH
		wdh := eh
		if wdh < 0 {
			wdh = -(wdh + 1)
		}
		wd1h := (564 * e.bandHigh.det) >> 12
		mih := 1
		if wdh >= wd1h {
			mih = 2
		}

		var ihigh int
		if eh < 0 {
			ihigh = g722IHN[mih]
		} else {
			ihigh = g722IHP[mih]
		}

		// Block 2H, INVQAH
		dhigh := (e.bandHigh.det * g722QM2[ihigh]) >> 15

		// Block 3H, LOGSCH
		ih2 := g722RH2[ihigh]
		wdh2 := (e.bandHigh.nb * 127) >> 7
		e.bandHigh.nb = wdh2 + g722WH[ih2]
		if e.bandHigh.nb < 0 {
			e.bandHigh.nb = 0
		} else if e.bandHigh.nb > 22528 {
			e.bandHigh.nb = 22528
		}

		// Block 3H, SCALEH
		wd1h = (e.bandHigh.nb >> 6) & 31
		wd2h := 10 - (e.bandHigh.nb >> 11)
		if wd2h < 0 {
			wd3 = g722ILB[wd1h] << (-wd2h)
		} else {
			wd3 = g722ILB[wd1h] >> wd2h
		}
		e.bandHigh.det = wd3 << 2

		g722Block4(&e.bandHigh, dhigh)

		data[i/2] = byte((ihigh << 6) | (ilow & 0x3F))
	}

	return data, nil
}

// Reset resets the encoder to its initial state.
func (e *G722Encoder) Reset() {
	e.bandLow = g722BandState{det: 32}
	e.bandHigh = g722BandState{det: 8}
	e.qmfX = [12]int{}
	e.qmfY = [12]int{}
	e.qmfPtr = 0
}

// ---------------------------------------------------------------------------
// Sample rate conversion helpers
// ---------------------------------------------------------------------------

// Upsample8to16 converts 8kHz 16-bit LE PCM bytes to 16kHz int16 samples
// by duplicating each sample (zero-order hold).
func Upsample8to16(pcm8k []byte) []int16 {
	numSamples := len(pcm8k) / 2
	out := make([]int16, numSamples*2)
	for i := 0; i < numSamples; i++ {
		s := int16(binary.LittleEndian.Uint16(pcm8k[i*2:]))
		out[i*2] = s
		out[i*2+1] = s
	}
	return out
}

// Downsample16to8 converts 16kHz int16 samples to 8kHz 16-bit LE PCM bytes
// by taking every other sample.
func Downsample16to8(samples16k []int16) []byte {
	numOut := len(samples16k) / 2
	out := make([]byte, numOut*2)
	for i := 0; i < numOut; i++ {
		binary.LittleEndian.PutUint16(out[i*2:], uint16(samples16k[i*2]))
	}
	return out
}

// ---------------------------------------------------------------------------
// io.Reader / io.Writer adapters for pipeline integration
// ---------------------------------------------------------------------------

// G722DecoderReader reads G.722 encoded data from Source and produces 8kHz
// 16-bit LE PCM. Internally decodes to 16kHz then downsamples.
type G722DecoderReader struct {
	Source io.Reader
	dec    *G722Decoder
	buf    []byte // read buffer for encoded data
	remain []byte // leftover decoded PCM not yet returned
}

// NewG722DecoderReader creates a reader that decodes G.722 to 8kHz PCM.
func NewG722DecoderReader(source io.Reader) *G722DecoderReader {
	return &G722DecoderReader{
		Source: source,
		dec:    NewG722Decoder(),
		buf:    make([]byte, RTPBufSize),
	}
}

func (d *G722DecoderReader) Read(b []byte) (int, error) {
	// Return leftover data first.
	if len(d.remain) > 0 {
		n := copy(b, d.remain)
		d.remain = d.remain[n:]
		return n, nil
	}

	// Read encoded G.722 data from source.
	n, err := d.Source.Read(d.buf)
	if err != nil {
		return 0, err
	}

	// Decode to 16kHz PCM.
	samples16k, err := d.dec.Decode(d.buf[:n])
	if err != nil {
		return 0, err
	}

	// Downsample to 8kHz PCM bytes.
	pcm8k := Downsample16to8(samples16k)

	nc := copy(b, pcm8k)
	if nc < len(pcm8k) {
		d.remain = pcm8k[nc:]
	}
	return nc, nil
}

// G722EncoderWriter accepts 8kHz 16-bit LE PCM and writes G.722 encoded data.
type G722EncoderWriter struct {
	Writer io.Writer
	enc    *G722Encoder
}

// NewG722EncoderWriter creates a writer that encodes 8kHz PCM to G.722.
func NewG722EncoderWriter(writer io.Writer) *G722EncoderWriter {
	return &G722EncoderWriter{
		Writer: writer,
		enc:    NewG722Encoder(),
	}
}

func (e *G722EncoderWriter) Write(pcm []byte) (int, error) {
	// Upsample 8kHz → 16kHz.
	samples16k := Upsample8to16(pcm)

	// Encode to G.722.
	encoded, err := e.enc.Encode(samples16k)
	if err != nil {
		return 0, err
	}

	if _, err := e.Writer.Write(encoded); err != nil {
		return 0, err
	}
	return len(pcm), nil
}
