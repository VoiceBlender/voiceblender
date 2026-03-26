package codec

import (
	"math"
	"testing"
)

func TestG722DecoderBasic(t *testing.T) {
	dec := NewG722Decoder()

	// Empty input produces empty output.
	samples, err := dec.Decode([]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 0 {
		t.Fatalf("expected 0 samples, got %d", len(samples))
	}

	// Single byte produces 2 samples.
	samples, err = dec.Decode([]byte{0x00})
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 2 {
		t.Fatalf("expected 2 samples, got %d", len(samples))
	}

	// 80 bytes (10ms at 64kbps) produces 160 samples.
	dec.Reset()
	data := make([]byte, 80)
	samples, err = dec.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 160 {
		t.Fatalf("expected 160 samples, got %d", len(samples))
	}
}

func TestG722EncoderBasic(t *testing.T) {
	enc := NewG722Encoder()

	// Empty input produces empty output.
	data, err := enc.Encode([]int16{})
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 0 {
		t.Fatalf("expected 0 bytes, got %d", len(data))
	}

	// 2 samples produce 1 byte.
	data, err = enc.Encode([]int16{0, 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 1 {
		t.Fatalf("expected 1 byte, got %d", len(data))
	}

	// 160 samples (10ms at 16kHz) produce 80 bytes.
	enc.Reset()
	samples := make([]int16, 160)
	data, err = enc.Encode(samples)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 80 {
		t.Fatalf("expected 80 bytes, got %d", len(data))
	}
}

func TestG722SilenceRoundTrip(t *testing.T) {
	enc := NewG722Encoder()
	dec := NewG722Decoder()

	samples := make([]int16, 160)
	encoded, err := enc.Encode(samples)
	if err != nil {
		t.Fatal(err)
	}

	decoded, err := dec.Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 160 {
		t.Fatalf("expected 160 decoded samples, got %d", len(decoded))
	}

	for i, s := range decoded {
		if s > 5000 || s < -5000 {
			t.Fatalf("decoded silence sample[%d]=%d exceeds threshold", i, s)
		}
	}
}

func TestG722ToneRoundTrip(t *testing.T) {
	enc := NewG722Encoder()
	dec := NewG722Decoder()

	// Generate and process 5 frames to let adaptive quantizer converge.
	for frame := 0; frame < 5; frame++ {
		samples := make([]int16, 160)
		for i := range samples {
			sampleIdx := frame*160 + i
			samples[i] = int16(10000 * math.Sin(2*math.Pi*1000*float64(sampleIdx)/16000))
		}

		encoded, err := enc.Encode(samples)
		if err != nil {
			t.Fatal(err)
		}

		decoded, err := dec.Decode(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if len(decoded) != 160 {
			t.Fatalf("frame %d: expected 160 samples, got %d", frame, len(decoded))
		}

		// Check energy on last frame (after convergence).
		if frame == 4 {
			var origEnergy, decEnergy float64
			for i := range samples {
				origEnergy += float64(samples[i]) * float64(samples[i])
				decEnergy += float64(decoded[i]) * float64(decoded[i])
			}

			// Energy should be within same order of magnitude.
			if math.Abs(origEnergy-decEnergy) > origEnergy*2 {
				t.Fatalf("energy mismatch: original=%f decoded=%f", origEnergy, decEnergy)
			}
		}
	}
}

func TestG722MultipleFrames(t *testing.T) {
	enc := NewG722Encoder()
	dec := NewG722Decoder()

	for frame := 0; frame < 10; frame++ {
		samples := make([]int16, 160)
		for i := range samples {
			samples[i] = int16(5000 * math.Sin(2*math.Pi*500*float64(frame*160+i)/16000))
		}

		encoded, err := enc.Encode(samples)
		if err != nil {
			t.Fatal(err)
		}
		if len(encoded) != 80 {
			t.Fatalf("frame %d: expected 80 encoded bytes, got %d", frame, len(encoded))
		}

		decoded, err := dec.Decode(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if len(decoded) != 160 {
			t.Fatalf("frame %d: expected 160 decoded samples, got %d", frame, len(decoded))
		}
	}
}

func TestG722Reset(t *testing.T) {
	enc := NewG722Encoder()
	dec := NewG722Decoder()

	// Encode/decode some data to change state.
	_, _ = enc.Encode([]int16{1000, 2000, 3000, 4000})
	_, _ = dec.Decode([]byte{0x55, 0xAA})

	enc.Reset()
	dec.Reset()

	// After reset, encoding silence should work normally.
	data, err := enc.Encode(make([]int16, 160))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 80 {
		t.Fatalf("expected 80 bytes after reset, got %d", len(data))
	}

	samples, err := dec.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 160 {
		t.Fatalf("expected 160 samples after reset, got %d", len(samples))
	}
}

func TestUpsample8to16(t *testing.T) {
	// 4 samples of 8kHz PCM (8 bytes)
	pcm := []byte{
		0x00, 0x10, // 4096
		0x00, 0x20, // 8192
		0x00, 0xF0, // -4096 (0xF000 as int16)
		0x00, 0x00, // 0
	}

	out := Upsample8to16(pcm)
	if len(out) != 8 {
		t.Fatalf("expected 8 samples, got %d", len(out))
	}

	// Each 8kHz sample should be duplicated.
	expected := []int16{4096, 4096, 8192, 8192, -4096, -4096, 0, 0}
	for i, s := range out {
		if s != expected[i] {
			t.Fatalf("sample[%d]: expected %d, got %d", i, expected[i], s)
		}
	}
}

func TestDownsample16to8(t *testing.T) {
	// 8 samples of 16kHz (take every other → 4 samples of 8kHz)
	samples := []int16{100, 200, 300, 400, 500, 600, 700, 800}

	pcm := Downsample16to8(samples)
	if len(pcm) != 8 { // 4 samples * 2 bytes
		t.Fatalf("expected 8 bytes, got %d", len(pcm))
	}

	// Should keep samples at indices 0, 2, 4, 6.
	expectedSamples := []int16{100, 300, 500, 700}
	for i, exp := range expectedSamples {
		got := int16(uint16(pcm[i*2]) | uint16(pcm[i*2+1])<<8)
		if got != exp {
			t.Fatalf("sample[%d]: expected %d, got %d", i, exp, got)
		}
	}
}

func TestG722UpsampleEncodeDecodeDownsample(t *testing.T) {
	// Full pipeline test: 8kHz PCM → upsample → G.722 encode → decode → downsample → 8kHz PCM
	enc := NewG722Encoder()
	dec := NewG722Decoder()

	// Warm up with several frames so the adaptive quantizer converges.
	for frame := 0; frame < 10; frame++ {
		// Generate 20ms of 8kHz PCM (160 samples = 320 bytes).
		pcm8k := make([]byte, 320)
		for i := 0; i < 160; i++ {
			sampleIdx := frame*160 + i
			s := int16(5000 * math.Sin(2*math.Pi*440*float64(sampleIdx)/8000))
			pcm8k[i*2] = byte(s)
			pcm8k[i*2+1] = byte(s >> 8)
		}

		samples16k := Upsample8to16(pcm8k)
		if len(samples16k) != 320 {
			t.Fatalf("upsample: expected 320 samples, got %d", len(samples16k))
		}

		encoded, err := enc.Encode(samples16k)
		if err != nil {
			t.Fatal(err)
		}
		if len(encoded) != 160 {
			t.Fatalf("encode: expected 160 bytes, got %d", len(encoded))
		}

		decoded, err := dec.Decode(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if len(decoded) != 320 {
			t.Fatalf("decode: expected 320 samples, got %d", len(decoded))
		}

		pcm8kOut := Downsample16to8(decoded)
		if len(pcm8kOut) != 320 {
			t.Fatalf("downsample: expected 320 bytes, got %d", len(pcm8kOut))
		}
	}
}
