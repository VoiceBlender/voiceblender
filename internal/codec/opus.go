package codec

import (
	"encoding/binary"
	"fmt"

	"github.com/thesyncim/gopus"
)

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
	return &OpusEncoder{
		enc: enc,
		buf: make([]byte, 4000),
	}, nil
}

// Encode encodes 48kHz int16 PCM samples to an Opus packet.
func (e *OpusEncoder) Encode(samples []int16) ([]byte, error) {
	n, err := e.enc.EncodeInt16(samples, e.buf)
	if err != nil {
		return nil, err
	}
	out := make([]byte, n)
	copy(out, e.buf[:n])
	return out, nil
}

// Reset resets the encoder state.
func (e *OpusEncoder) Reset() {
	e.enc.Reset()
}

// OpusDecoder wraps a gopus Decoder for 48kHz mono.
type OpusDecoder struct {
	dec    *gopus.Decoder
	pcmBuf []int16 // reusable decode buffer
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

// Decode decodes an Opus packet to 48kHz int16 PCM samples.
func (d *OpusDecoder) Decode(data []byte) ([]int16, error) {
	// A 1-byte Opus packet (TOC only, frame code 0, 0 bytes of frame data)
	// is a valid DTX silence indicator per RFC 6716 §3.2.1. The gopus
	// decoder doesn't handle 0-byte CELT frames, so output silence directly.
	if len(data) == 1 && (data[0]&0x03) == 0 {
		toc := data[0]
		// Extract frame size from TOC config (bits 7-3).
		config := toc >> 3
		frameSizeMs := opusFrameSizeMs(config)
		samples := 48 * frameSizeMs // 48kHz → 48 samples per ms
		return make([]int16, samples), nil
	}

	n, err := d.dec.DecodeInt16(data, d.pcmBuf)
	if err != nil {
		return nil, err
	}
	out := make([]int16, n)
	copy(out, d.pcmBuf[:n])
	return out, nil
}

// opusFrameSizeMs returns the frame duration in ms for an Opus TOC config value.
// See RFC 6716 Table 2. The 2.5ms CELT sizes are rounded up to 3ms (rare in VoIP).
func opusFrameSizeMs(config uint8) int {
	switch {
	case config <= 3: // SILK NB
		return []int{10, 20, 40, 60}[config]
	case config <= 7: // SILK MB
		return []int{10, 20, 40, 60}[config-4]
	case config <= 11: // SILK WB
		return []int{10, 20, 40, 60}[config-8]
	case config <= 13: // Hybrid SWB
		return []int{10, 20}[config-12]
	case config <= 15: // Hybrid FB
		return []int{10, 20}[config-14]
	case config <= 19: // CELT NB
		return []int{3, 5, 10, 20}[config-16]
	case config <= 23: // CELT WB
		return []int{3, 5, 10, 20}[config-20]
	case config <= 27: // CELT SWB
		return []int{3, 5, 10, 20}[config-24]
	case config <= 31: // CELT FB
		return []int{3, 5, 10, 20}[config-28]
	default:
		return 20
	}
}

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
