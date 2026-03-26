package codec

import "github.com/zaf/g711"

// PCMAEncoder encodes 16-bit linear PCM to G.711 A-law.
// Stateless — one byte per sample.
type PCMAEncoder struct{}

func (e *PCMAEncoder) Encode(samples []int16) ([]byte, error) {
	out := make([]byte, len(samples))
	for i, s := range samples {
		out[i] = g711.EncodeAlawFrame(s)
	}
	return out, nil
}

func (e *PCMAEncoder) Reset() {}

// PCMADecoder decodes G.711 A-law to 16-bit linear PCM.
// Stateless — one sample per byte.
type PCMADecoder struct{}

func (d *PCMADecoder) Decode(data []byte) ([]int16, error) {
	out := make([]int16, len(data))
	for i, b := range data {
		out[i] = g711.DecodeAlawFrame(b)
	}
	return out, nil
}

func (d *PCMADecoder) Reset() {}
