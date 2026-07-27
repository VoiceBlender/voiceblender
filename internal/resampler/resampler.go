// Copyright (C) 2007-2008 Jean-Marc Valin
// Copyright (C) 2008      Thorvald Natvig
// Copyright (C) 2013      Oov
//
// Arbitrary resampling code
//
// Redistribution and use in source and binary forms, with or without
// modification, are permitted provided that the following conditions are
// met:
//
// 1. Redistributions of source code must retain the above copyright notice,
// this list of conditions and the following disclaimer.
//
// 2. Redistributions in binary form must reproduce the above copyright
// notice, this list of conditions and the following disclaimer in the
// documentation and/or other materials provided with the distribution.
//
// 3. The name of the author may not be used to endorse or promote products
// derived from this software without specific prior written permission.
//
// THIS SOFTWARE IS PROVIDED BY THE AUTHOR ``AS IS'' AND ANY EXPRESS OR
// IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE IMPLIED WARRANTIES
// OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE
// DISCLAIMED. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR ANY DIRECT,
// INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES
// (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR
// SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION)
// HOWEVER CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT,
// STRICT LIABILITY, OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN
// ANY WAY OUT OF THE USE OF THIS SOFTWARE, EVEN IF ADVISED OF THE
// POSSIBILITY OF SUCH DAMAGE.

// Package resampler implements audio resampler.
//
// This is a port of the Opus-tools( http://git.xiph.org/?p=opus-tools.git ) audio resampler to the pure Go.
//
// Vendored from github.com/oov/audio/resampler (MIT, see LICENSE in this
// directory) to avoid depending on an unmaintained module. The per-file
// copyright headers are the upstream Speex/Opus resampler's and are retained.
package resampler

import (
	"math"
)

const bufferSize = 160

type channelState struct {
	lastSample   int
	sampFracNum  int
	magicSamples int
	mem          []float64
}

type Resampler struct {
	numRate int
	denRate int

	quality     *quality
	filtLen     int
	intAdvance  int
	fracAdvance int
	cutoff      float64
	oversample  int

	initialised bool
	started     bool
	skipZeros   bool

	channels  []channelState
	sincTable []float64
	resampler func(channelIndex int, in []float64, out []float64) int
}

func Resample64(in []float64, inSampleRate int, out []float64, outSampleRate int, quality int) (read int, written int) {
	return NewWithSkipZeros(1, inSampleRate, outSampleRate, quality).ProcessFloat64(0, in, out)
}

func Resample32(in []float32, inSampleRate int, out []float32, outSampleRate int, quality int) (read int, written int) {
	return NewWithSkipZeros(1, inSampleRate, outSampleRate, quality).ProcessFloat32(0, in, out)
}

func New(channels int, inSampleRate, outSampleRate int, quality int) *Resampler {
	if channels < 1 {
		panic("you must have at least one channel")
	}
	r := &Resampler{
		cutoff:   1.0,
		channels: make([]channelState, channels),
	}

	r.setQuality(quality)
	r.setSampleRate(inSampleRate, outSampleRate)
	r.updateFilter()
	r.initialised = true
	return r
}

func NewWithSkipZeros(channels int, inSampleRate, outSampleRate int, quality int) *Resampler {
	r := New(channels, inSampleRate, outSampleRate, quality)
	r.skipZeros = true
	return r
}

// cannot change quality on running in now implementation.
func (r *Resampler) setQuality(q int) {
	if q < 0 || q > 10 {
		panic("invalid quality value")
	}

	if r.quality == &qualityMap[q] {
		return
	}
	r.quality = &qualityMap[q]
	if r.initialised {
		r.updateFilter()
	}
}

func imin(a, b int) int {
	if a < b {
		return a
	} else {
		return b
	}
}

// cannot change sample on running in now implementation.
func (r *Resampler) setSampleRate(in int, out int) {
	ratioNum := in
	ratioDen := out

	// FIXME: This is terribly inefficient, but who cares (at least for now)?
	for fact := 2; fact <= imin(ratioNum, ratioDen); fact++ {
		for (ratioNum%fact == 0) && (ratioDen%fact == 0) {
			ratioNum /= fact
			ratioDen /= fact
		}
	}

	if r.numRate == ratioNum && r.denRate == ratioDen {
		return
	}

	if r.denRate > 0 {
		for i := range r.channels {
			ch := &r.channels[i]
			ch.sampFracNum = ch.sampFracNum * ratioDen / r.denRate
			// Safety net
			if ch.sampFracNum >= ratioDen {
				ch.sampFracNum = ratioDen - 1
			}
		}
	}

	r.numRate = ratioNum
	r.denRate = ratioDen

	if r.initialised {
		r.updateFilter()
	}
}

func (r *Resampler) ProcessFloat64(channelIndex int, in []float64, out []float64) (read int, written int) {
	if r.skipZeros {
		for i := range r.channels {
			r.channels[i].lastSample = r.InputLatency()
		}
		r.skipZeros = false
	}

	ch := &r.channels[channelIndex]
	x := ch.mem
	filtOffs := r.filtLen - 1
	iLen, oLen, xLen := len(in), len(out), len(x)-filtOffs
	read, written = iLen, oLen

	if ch.magicSamples != 0 {
		oLen -= r.magic(channelIndex, out)
	}

	if ch.magicSamples == 0 {
		for iLen != 0 && oLen != 0 {
			ichunk, ochunk := imin(xLen, iLen), 0
			if in != nil {
				copy(x[filtOffs:], in[:ichunk])
			} else {
				for j := filtOffs; j < ichunk+filtOffs; j++ {
					x[j] = 0
				}
			}
			ichunk, ochunk = r.processNative(channelIndex, ichunk, out)
			iLen -= ichunk
			oLen -= ochunk
			out = out[ochunk:]
			if in != nil {
				in = in[ichunk:]
			}
		}
	}
	read -= iLen
	written -= oLen
	return
}

func (r *Resampler) ProcessFloat32(channelIndex int, in []float32, out []float32) (read int, written int) {
	const stackSize = 1024
	var stack [stackSize]float64

	if r.skipZeros {
		for i := range r.channels {
			r.channels[i].lastSample = r.InputLatency()
		}
		r.skipZeros = false
	}

	ch := &r.channels[channelIndex]
	x := ch.mem
	filtOffs := r.filtLen - 1
	iLen, oLen := len(in), len(out)
	xLen, yLen := len(x)-filtOffs, stackSize
	read, written = iLen, oLen

	if ch.magicSamples != 0 {
		m := r.magic(channelIndex, stack[:imin(yLen, oLen)])
		oLen -= m
		for i, s := range stack[:m] {
			out[i] = float32(s)
		}
		out = out[m:]
	}

	if ch.magicSamples == 0 {
		for iLen != 0 && oLen != 0 {
			ichunk, ochunk := imin(xLen, iLen), imin(yLen, oLen)
			if in != nil {
				for i, s := range in[:ichunk] {
					x[filtOffs+i] = float64(s)
				}
			} else {
				for j := filtOffs; j < ichunk+filtOffs; j++ {
					x[j] = 0
				}
			}
			ichunk, ochunk = r.processNative(channelIndex, ichunk, stack[:ochunk])
			iLen -= ichunk
			oLen -= ochunk
			for i, s := range stack[:ochunk] {
				out[i] = float32(s)
			}
			out = out[ochunk:]
			if in != nil {
				in = in[ichunk:]
			}
		}
	}
	read -= iLen
	written -= oLen
	return
}

func (r *Resampler) processNative(channelIndex int, inLen int, out []float64) (inLenRet int, outLenRet int) {
	ch := &r.channels[channelIndex]
	r.started = true

	outLenRet = r.resampler(channelIndex, ch.mem[:inLen], out)
	if ch.lastSample < inLen {
		inLenRet = ch.lastSample
	} else {
		inLenRet = inLen
	}
	ch.lastSample -= inLenRet
	copy(ch.mem, ch.mem[inLenRet:inLenRet+r.filtLen-1])
	return
}

func (r *Resampler) magic(channelIndex int, out []float64) (outWritten int) {
	ch := &r.channels[channelIndex]
	n := r.filtLen - 1

	inLen, outLen := r.processNative(channelIndex, ch.magicSamples, out)

	ch.magicSamples -= inLen

	// If we couldn't process all "magic" input samples, save the rest for next time
	if ch.magicSamples != 0 {
		copy(ch.mem[n:n+ch.magicSamples], ch.mem[n+inLen:])
	}
	return outLen
}

func computeFunc(x float64, windowFunc *kaiserTable) float64 {
	y := x * float64(windowFunc.oversample)
	ind := int(math.Floor(y))
	frac := y - float64(ind)
	fracx2 := frac * frac
	fracx3 := fracx2 * frac
	fracx2mul0_5 := 0.5 * fracx2
	fracx3mul0_16 := 0.1666666667 * fracx3
	// Compute interpolation coefficients. I'm not sure whether this corresponds to cubic interpolation but I know it's MMSE-optimal on a sinc
	i3 := -0.1666666667*frac + fracx3mul0_16
	i2 := frac + fracx2mul0_5 - 0.5*fracx3
	// interp[2] = 1.f - 0.5f*frac - frac*frac + 0.5f*frac*frac*frac;
	i0 := -0.3333333333*frac + fracx2mul0_5 - fracx3mul0_16
	// Just to make sure we don't have rounding problems
	i1 := 1.0 - i3 - i2 - i0
	return i0*windowFunc.table[ind] + i1*windowFunc.table[ind+1] + i2*windowFunc.table[ind+2] + i3*windowFunc.table[ind+3]
}

// The slow way of computing a sinc for the table. Should improve that some day
func sinc(cutoff float64, x float64, n float64, windowFunc *kaiserTable) float64 {
	xabs := math.Abs(x)
	if xabs < 1e-6 {
		return cutoff
	} else if xabs > 0.5*n {
		return 0
	}
	// FIXME: Can it really be any slower than this?
	xx := x * cutoff * math.Pi
	return cutoff * math.Sin(xx) / xx * computeFunc(2.0*xabs/n, windowFunc)
}

func (r *Resampler) resamplerBasicDirect(channelIndex int, in []float64, out []float64) int {
	ch := &r.channels[channelIndex]
	n := r.filtLen
	outSample := 0
	lastSample := ch.lastSample
	sampFracNum := ch.sampFracNum
	sincTable := r.sincTable
	intAdvance := r.intAdvance
	fracAdvance := r.fracAdvance
	denRate := r.denRate

	for lastSample < len(in) && outSample < len(out) {
		sinct := sincTable[sampFracNum*n : sampFracNum*n+n]
		var sum float64
		for j, s := range in[lastSample : lastSample+n] {
			sum += sinct[j] * s
		}

		out[outSample] = sum
		outSample++
		lastSample += intAdvance
		sampFracNum += fracAdvance
		if sampFracNum >= denRate {
			sampFracNum -= denRate
			lastSample++
		}
	}
	ch.lastSample = lastSample
	ch.sampFracNum = sampFracNum
	return outSample
}

func cubicCoef(frac float64) (float64, float64, float64, float64) {
	fracx2 := frac * frac
	fracx3 := fracx2 * frac
	fracx2mul0_5 := 0.5 * fracx2
	fracx3mul0_16 := 0.1666666667 * fracx3
	// Compute interpolation coefficients. I'm not sure whether this corresponds to cubic interpolation but I know it's MMSE-optimal on a sinc
	i0 := -0.1666666667*frac + fracx3mul0_16
	i1 := frac + fracx2mul0_5 - 0.5*fracx3
	// interp[2] = 1.f - 0.5f*frac - frac*frac + 0.5f*frac*frac*frac;
	i3 := -0.3333333333*frac + fracx2mul0_5 - fracx3mul0_16
	// Just to make sure we don't have rounding problems
	i2 := 1.0 - i0 - i1 - i3
	return i0, i1, i2, i3
}

func (r *Resampler) resamplerBasicInterpolate(channelIndex int, in []float64, out []float64) int {
	ch := &r.channels[channelIndex]
	n := r.filtLen
	outSample := 0
	lastSample := ch.lastSample
	sampFracNum := ch.sampFracNum
	intAdvance := r.intAdvance
	fracAdvance := r.fracAdvance
	denRate := r.denRate

	for lastSample < len(in) && outSample < len(out) {
		offset := sampFracNum * r.oversample / r.denRate
		frac := float64((sampFracNum*r.oversample)%r.denRate) / float64(r.denRate)
		var accum0, accum1, accum2, accum3 float64
		for j, s := range in[lastSample : lastSample+n] {
			t := 4 + (j+1)*r.oversample - offset
			accum0 += s * r.sincTable[t-2]
			accum1 += s * r.sincTable[t-1]
			accum2 += s * r.sincTable[t]
			accum3 += s * r.sincTable[t+1]
		}
		i0, i1, i2, i3 := cubicCoef(frac)
		out[outSample] = i0*accum0 + i1*accum1 + i2*accum2 + i3*accum3
		outSample++
		lastSample += intAdvance
		sampFracNum += fracAdvance
		if sampFracNum >= denRate {
			sampFracNum -= denRate
			lastSample++
		}
	}
	ch.lastSample = lastSample
	ch.sampFracNum = sampFracNum
	return outSample
}

func (r *Resampler) updateFilter() {
	oldLength := r.filtLen
	r.oversample = r.quality.oversample
	r.filtLen = r.quality.baseLength

	if r.numRate > r.denRate {
		// down-sampling
		r.cutoff = r.quality.downsampleBandwidth * float64(r.denRate) / float64(r.numRate)
		// FIXME: divide the numerator and denominator by a certain amount if they're too large
		r.filtLen = r.filtLen * r.numRate / r.denRate
		// Round up to make sure we have a multiple of 8
		r.filtLen = ((r.filtLen - 1) & (^int(0x7))) + 8
		if r.denRate<<1 < r.numRate {
			r.oversample >>= 1
		}
		if r.denRate<<2 < r.numRate {
			r.oversample >>= 1
		}
		if r.denRate<<3 < r.numRate {
			r.oversample >>= 1
		}
		if r.denRate<<4 < r.numRate {
			r.oversample >>= 1
		}
		if r.oversample < 1 {
			r.oversample = 1
		}
	} else {
		// up-sampling
		r.cutoff = r.quality.upsampleBandwidth
	}

	// Choose the resampling type that requires the least amount of memory
	if r.denRate <= 16*(r.oversample+8) {
		if r.sincTable == nil || len(r.sincTable) < r.filtLen*r.denRate {
			r.sincTable = make([]float64, r.filtLen*r.denRate)
		}
		for i := 0; i < r.denRate; i++ {
			for j := 0; j < r.filtLen; j++ {
				r.sincTable[i*r.filtLen+j] = sinc(
					r.cutoff,
					float64(j-(r.filtLen>>1)+1)-float64(i)/float64(r.denRate),
					float64(r.filtLen),
					r.quality.table,
				)
			}
		}
		r.resampler = r.resamplerBasicDirect
	} else {
		if r.sincTable == nil || len(r.sincTable) < r.filtLen*r.oversample+8 {
			r.sincTable = make([]float64, r.filtLen*r.oversample+8)
		}
		for i := -4; i < r.oversample*r.filtLen+4; i++ {
			r.sincTable[i+4] = sinc(
				r.cutoff,
				float64(i)/float64(r.oversample)-float64(r.filtLen>>1),
				float64(r.filtLen),
				r.quality.table,
			)
		}
		r.resampler = r.resamplerBasicInterpolate
	}

	r.intAdvance = r.numRate / r.denRate
	r.fracAdvance = r.numRate % r.denRate

	// Here's the place where we update the filter memory to take into account
	// the change in filter length. It's probably the messiest part of the code
	// due to handling of lots of corner cases.
	switch {
	case r.channels[0].mem == nil || !r.started:
		size := r.filtLen - 1 + bufferSize
		for i := range r.channels {
			r.channels[i].mem = make([]float64, size)
		}
	case r.filtLen > oldLength:
		// Increasing the filter length mid-stream is unsupported upstream;
		// unreachable here since the resampler is built once per fixed rate pair.
		panic("not implemented")
	case r.filtLen < oldLength:
		panic("not implemented")
		//      spx_uint32_t i;
		//      /* Reduce filter length, this a bit tricky. We need to store some of the memory as "magic"
		//         samples so they can be used directly as input the next time(s) */
		//      for (i=0;i<st->nb_channels;i++)
		//      {
		//         spx_uint32_t j;
		//         spx_uint32_t old_magic = st->magic_samples[i];
		//         st->magic_samples[i] = (old_length - st->filt_len)/2;
		//         /* We must copy some of the memory that's no longer used */
		//         /* Copy data going backward */
		//         for (j=0;j<st->filt_len-1+st->magic_samples[i]+old_magic;j++)
		//            st->mem[i*st->mem_alloc_size+j] = st->mem[i*st->mem_alloc_size+j+st->magic_samples[i]];
		//         st->magic_samples[i] += old_magic;
		//      }
	}
}

func (r *Resampler) InputLatency() int {
	return r.filtLen >> 1
}

func (r *Resampler) OutputLatency() int {
	return ((r.filtLen>>1)*r.denRate + (r.numRate >> 1)) / r.numRate
}
