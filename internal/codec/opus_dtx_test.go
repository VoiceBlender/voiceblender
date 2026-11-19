package codec

import "testing"

// A TOC-only packet carries empty frames; the decoder must still emit exactly
// the duration the TOC describes so the leg's timeline stays in step.
func TestOpusDecodeDTXDuration(t *testing.T) {
	cases := []struct {
		name string
		toc  byte
		want int
	}{
		{"SILK NB 10ms", 0<<3 | 0, 480},
		{"SILK NB 20ms", 1<<3 | 0, 960},
		{"SILK WB 60ms", 11<<3 | 0, 2880},
		{"Hybrid FB 20ms", 15<<3 | 0, 960},
		{"CELT NB 2.5ms", 16<<3 | 0, 120},
		{"CELT FB 2.5ms", 28<<3 | 0, 120},
		{"CELT FB 5ms", 29<<3 | 0, 240},
		{"CELT FB 20ms", 31<<3 | 0, 960},
		{"two empty frames", 1<<3 | 1, 1920},
		{"two empty CELT 2.5ms frames", 16<<3 | 1, 240},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dec, err := NewOpusDecoder()
			if err != nil {
				t.Fatal(err)
			}
			pcm, err := dec.Decode([]byte{tc.toc})
			if err != nil {
				t.Fatal(err)
			}
			if len(pcm) != tc.want {
				t.Fatalf("got %d samples, want %d", len(pcm), tc.want)
			}
		})
	}
}

// DTX between voiced frames must not break decoding of what follows.
func TestOpusDecodeDTXThenSpeech(t *testing.T) {
	enc, err := NewOpusEncoder()
	if err != nil {
		t.Fatal(err)
	}
	dec, _ := NewOpusDecoder()
	for f := range 10 {
		payload, err := enc.Encode(voiced(48000, f*opusGapSamples, opusGapSamples))
		if err != nil {
			t.Fatal(err)
		}
		if f == 4 || f == 5 {
			payload = []byte{payload[0] &^ 0x03}
		}
		pcm, err := dec.Decode(payload)
		if err != nil {
			t.Fatalf("frame %d: %v", f, err)
		}
		if len(pcm) != opusGapSamples {
			t.Fatalf("frame %d: got %d samples, want %d", f, len(pcm), opusGapSamples)
		}
	}
}

func TestOpusDecodeDTXIsSilent(t *testing.T) {
	enc, err := NewOpusEncoder()
	if err != nil {
		t.Fatal(err)
	}
	dec, _ := NewOpusDecoder()
	for f := range 4 {
		payload, err := enc.Encode(voiced(48000, f*opusGapSamples, opusGapSamples))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := dec.Decode(payload); err != nil {
			t.Fatal(err)
		}
	}
	pcm, err := dec.Decode([]byte{15 << 3})
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range pcm {
		if s != 0 {
			t.Fatalf("sample %d is %d, want silence", i, s)
		}
	}
}
