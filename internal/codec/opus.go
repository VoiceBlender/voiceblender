package codec

import (
	"encoding/binary"
	"fmt"
	"runtime/debug"

	"github.com/thesyncim/gopus"
)

// Opus target bitrate bounds (bps), and the default for a new encoder.
const (
	MinOpusBitrate     = 6000
	MaxOpusBitrate     = 510000
	DefaultOpusBitrate = 24000
)

// OpusParams shapes an Opus encoder for one receiver.
type OpusParams struct {
	// Bitrate is the target in bits per second.
	Bitrate int
	// MaxPlaybackRate is the highest sample rate the receiver renders
	// (RFC 7587 maxplaybackrate); 0 means no limit.
	MaxPlaybackRate int
}

// OpusEncoder wraps a gopus Encoder for 48kHz mono VoIP.
type OpusEncoder struct {
	enc *gopus.Encoder
	buf []byte // reusable output buffer
}

// NewOpusEncoder creates a new Opus encoder configured for 48kHz mono VoIP.
func NewOpusEncoder() (*OpusEncoder, error) {
	enc, err := gopus.NewEncoder(gopus.EncoderConfig{
		SampleRate:  48000,
		Channels:    1,
		Application: gopus.ApplicationVoIP,
	})
	if err != nil {
		return nil, fmt.Errorf("gopus.NewEncoder: %w", err)
	}
	if err := enc.SetBitrate(DefaultOpusBitrate); err != nil {
		return nil, fmt.Errorf("gopus.SetBitrate: %w", err)
	}
	return &OpusEncoder{
		enc: enc,
		buf: make([]byte, 4000),
	}, nil
}

// Encode encodes 48kHz int16 PCM samples to an Opus packet. Panics in the
// underlying codec are converted to errors so one bad frame fails just the
// leg, not the process.
func (e *OpusEncoder) Encode(samples []int16) (out []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("opus encoder panic: %v\n%s", r, debug.Stack())
			out = nil
		}
	}()
	n, err := e.enc.EncodeInt16(samples, e.buf)
	if err != nil {
		return nil, err
	}
	out = make([]byte, n)
	copy(out, e.buf[:n])
	return out, nil
}

// Reset resets the encoder state.
func (e *OpusEncoder) Reset() {
	e.enc.Reset()
}

// SetBitrate adjusts the target bitrate in bits per second (gopus accepts
// 6_000..510_000).
func (e *OpusEncoder) SetBitrate(bps int) error {
	return e.enc.SetBitrate(bps)
}

// Bitrate returns the target bitrate in bits per second.
func (e *OpusEncoder) Bitrate() int { return e.enc.Bitrate() }

// SetMaxPlaybackRate caps the coded audio bandwidth at what a receiver
// rendering at hz can reproduce. 0 lifts the cap.
func (e *OpusEncoder) SetMaxPlaybackRate(hz int) error {
	return e.enc.SetMaxBandwidth(opusBandwidthFor(hz))
}

// Apply sets the bitrate and playback-rate cap together. A zero bitrate means
// DefaultOpusBitrate.
func (e *OpusEncoder) Apply(p OpusParams) error {
	if p.Bitrate <= 0 {
		p.Bitrate = DefaultOpusBitrate
	}
	if err := e.SetBitrate(p.Bitrate); err != nil {
		return err
	}
	return e.SetMaxPlaybackRate(p.MaxPlaybackRate)
}

// opusBandwidthFor maps a playback rate to the widest Opus bandwidth whose
// audio it can render (RFC 7587 §6.1).
func opusBandwidthFor(hz int) gopus.Bandwidth {
	switch {
	case hz <= 0:
		return gopus.BandwidthFullband
	case hz <= 8000:
		return gopus.BandwidthNarrowband
	case hz <= 12000:
		return gopus.BandwidthMediumband
	case hz <= 16000:
		return gopus.BandwidthWideband
	case hz <= 24000:
		return gopus.BandwidthSuperwideband
	default:
		return gopus.BandwidthFullband
	}
}

// OpusDecoder wraps a gopus Decoder for 48kHz mono.
type OpusDecoder struct {
	dec    *gopus.Decoder
	pcmBuf []int16   // reusable decode buffer
	fecBuf []float32 // reusable DecodeFEC buffer
}

// NewOpusDecoder creates a new Opus decoder configured for 48kHz mono.
func NewOpusDecoder() (*OpusDecoder, error) {
	cfg := gopus.DefaultDecoderConfig(48000, 1)
	dec, err := gopus.NewDecoder(cfg)
	if err != nil {
		return nil, fmt.Errorf("gopus.NewDecoder: %w", err)
	}
	return &OpusDecoder{
		dec:    dec,
		pcmBuf: make([]int16, 5760), // max Opus frame: 120ms at 48kHz
	}, nil
}

// Decode decodes an Opus packet to 48kHz int16 PCM samples. Mirrors Encode's
// panic-to-error conversion.
func (d *OpusDecoder) Decode(data []byte) (out []int16, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("opus decoder panic: %v\n%s", r, debug.Stack())
			out = nil
		}
	}()

	// DTX gives digital silence rather than the decoder's concealment fade.
	if len(data) == 1 && (data[0]&0x03) == 0 {
		return make([]int16, gopus.ParseTOC(data[0]).FrameSize), nil
	}

	n, err := d.dec.DecodeInt16(data, d.pcmBuf)
	if err != nil {
		return nil, err
	}
	out = make([]int16, n)
	copy(out, d.pcmBuf[:n])
	return out, nil
}

// opusGapSamples is the 20 ms a gap decoder fills, at 48 kHz.
const opusGapSamples = 960

// DecodeGap conceals one lost 20 ms frame with the decoder's own concealment.
func (d *OpusDecoder) DecodeGap(dst []int16) (out []int16, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("opus decoder panic: %v\n%s", r, debug.Stack())
			out = nil
		}
	}()
	out = growInt16(dst, opusGapSamples)
	n, err := d.dec.DecodeInt16(nil, out)
	if err != nil {
		return nil, err
	}
	return out[:n], nil
}

// DecodeFEC rebuilds the 20 ms before the packet next from its in-band
// redundancy. A packet that carries none gives concealment instead.
func (d *OpusDecoder) DecodeFEC(dst []int16, next []byte) (out []int16, err error) {
	if len(next) < 2 {
		return d.DecodeGap(dst)
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("opus decoder panic: %v\n%s", r, debug.Stack())
			out = nil
		}
	}()
	if d.fecBuf == nil {
		d.fecBuf = make([]float32, opusGapSamples)
	}
	n, err := d.dec.DecodeWithFEC(next, d.fecBuf, true)
	if err != nil {
		return nil, err
	}
	out = growInt16(dst, n)
	for i, v := range d.fecBuf[:n] {
		out[i] = clampInt16(v * 32768)
	}
	return out, nil
}

// InDTX is always false: silence in Opus DTX is not filled on a timer.
func (d *OpusDecoder) InDTX() bool { return false }

// Reset resets the decoder state.
func (d *OpusDecoder) Reset() {
	d.dec.Reset()
}

// Upsample8to48 converts 8kHz 16-bit LE PCM bytes to 48kHz int16 samples
// by duplicating each sample 6 times (zero-order hold).
func Upsample8to48(pcm8k []byte) []int16 {
	numSamples := len(pcm8k) / 2
	out := make([]int16, numSamples*6)
	for i := 0; i < numSamples; i++ {
		s := int16(binary.LittleEndian.Uint16(pcm8k[i*2:]))
		base := i * 6
		out[base] = s
		out[base+1] = s
		out[base+2] = s
		out[base+3] = s
		out[base+4] = s
		out[base+5] = s
	}
	return out
}

// Downsample48to8 converts 48kHz int16 samples to 8kHz 16-bit LE PCM bytes
// by taking every 6th sample.
func Downsample48to8(samples48k []int16) []byte {
	numOut := len(samples48k) / 6
	out := make([]byte, numOut*2)
	for i := 0; i < numOut; i++ {
		binary.LittleEndian.PutUint16(out[i*2:], uint16(samples48k[i*6]))
	}
	return out
}
