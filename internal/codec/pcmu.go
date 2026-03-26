package codec

import "github.com/zaf/g711"

// PCMUEncoder encodes 16-bit linear PCM to G.711 mu-law.
// Stateless — one byte per sample.
type PCMUEncoder struct{}

func (e *PCMUEncoder) Encode(samples []int16) ([]byte, error) {
	out := make([]byte, len(samples))
	for i, s := range samples {
		out[i] = g711.EncodeUlawFrame(s)
	}
	return out, nil
}

func (e *PCMUEncoder) Reset() {}

// PCMUDecoder decodes G.711 mu-law to 16-bit linear PCM.
// Stateless — one sample per byte.
type PCMUDecoder struct{}

func (d *PCMUDecoder) Decode(data []byte) ([]int16, error) {
	out := make([]int16, len(data))
	for i, b := range data {
		out[i] = g711.DecodeUlawFrame(b)
	}
	return out, nil
}

func (d *PCMUDecoder) Reset() {}
