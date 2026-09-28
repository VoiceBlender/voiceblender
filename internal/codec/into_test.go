package codec

import (
	"bytes"
	"math/rand"
	"slices"
	"testing"
)

func TestEncodeDecodeIntoMatchAllocatingPath(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	pcm := make([]int16, 320)
	for i := range pcm {
		pcm[i] = int16(r.Intn(65536) - 32768)
	}
	for _, ct := range []CodecType{CodecPCMU, CodecPCMA, CodecG722} {
		t.Run(ct.String(), func(t *testing.T) {
			encA, _ := NewEncoder(ct)
			encB, _ := NewEncoder(ct)
			decA, _ := NewDecoder(ct)
			decB, _ := NewDecoder(ct)
			if _, ok := encB.(IntoEncoder); !ok {
				t.Fatal("encoder does not implement IntoEncoder")
			}
			if _, ok := decB.(IntoDecoder); !ok {
				t.Fatal("decoder does not implement IntoDecoder")
			}
			var ebuf []byte
			var dbuf []int16
			// Several frames so stateful codecs (G.722) are compared mid-stream.
			for frame := 0; frame < 4; frame++ {
				want, err := encA.Encode(pcm)
				if err != nil {
					t.Fatal(err)
				}
				ebuf, err = EncodeInto(encB, ebuf, pcm)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(ebuf, want) {
					t.Fatalf("frame %d: EncodeInto differs from Encode", frame)
				}
				wantPCM, err := decA.Decode(want)
				if err != nil {
					t.Fatal(err)
				}
				dbuf, err = DecodeInto(decB, dbuf, ebuf)
				if err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(dbuf, wantPCM) {
					t.Fatalf("frame %d: DecodeInto differs from Decode", frame)
				}
			}
			if allocs := testing.AllocsPerRun(20, func() {
				ebuf, _ = EncodeInto(encB, ebuf, pcm)
				dbuf, _ = DecodeInto(decB, dbuf, ebuf)
			}); allocs > 0 {
				t.Fatalf("Into path allocated %.1f times per frame", allocs)
			}
		})
	}
}
