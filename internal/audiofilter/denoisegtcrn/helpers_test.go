package denoisegtcrn

import (
	"encoding/binary"
	"io"
	"math"
	"math/rand"
	"testing"
)

func install(t *testing.T) {
	t.Helper()
	if err := Install(); err != nil {
		t.Fatalf("Install: %v", err)
	}
	t.Cleanup(func() { Shutdown() })
}

func noise(n int, seed int64, amp float64) []int16 {
	rng := rand.New(rand.NewSource(seed))
	out := make([]int16, n)
	for i := range out {
		out[i] = clip(rng.NormFloat64() * amp)
	}
	return out
}

// bandLimited returns noise confined below fc at the given rate.
func bandLimited(n, rate int, seed int64, amp, fc float64) []int16 {
	rng := rand.New(rand.NewSource(seed))
	out := make([]int16, n)
	a := math.Exp(-2 * math.Pi * fc / float64(rate))
	y := 0.0
	for i := range out {
		y = (1-a)*rng.NormFloat64()*amp + a*y
		out[i] = clip(y / (1 - a) * 0.5)
	}
	return out
}

func clip(v float64) int16 {
	if v > 32767 {
		return 32767
	}
	if v < -32768 {
		return -32768
	}
	return int16(v)
}

func rms(s []int16) float64 {
	if len(s) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range s {
		sum += float64(v) * float64(v)
	}
	return math.Sqrt(sum / float64(len(s)))
}

func runFrames(t *testing.T, s *stream, in []int16) []int16 {
	t.Helper()
	buf := append([]int16(nil), in...)
	for off := 0; off+s.frame <= len(buf); off += s.frame {
		if err := s.process(buf[off : off+s.frame]); err != nil {
			t.Fatal(err)
		}
	}
	return buf
}

type sliceReader struct {
	data []byte
	n    int
	off  int
}

func (s *sliceReader) Read(p []byte) (int, error) {
	if s.off >= len(s.data) {
		return 0, io.EOF
	}
	n := min(s.n, len(p), len(s.data)-s.off)
	copy(p, s.data[s.off:s.off+n])
	s.off += n
	return n, nil
}

func toBytes(s []int16) []byte {
	b := make([]byte, len(s)*2)
	for i, v := range s {
		binary.LittleEndian.PutUint16(b[i*2:], uint16(v))
	}
	return b
}

func toSamples(b []byte) []int16 {
	s := make([]int16, len(b)/2)
	for i := range s {
		s[i] = int16(binary.LittleEndian.Uint16(b[i*2:]))
	}
	return s
}

func readAll(t *testing.T, r io.Reader) []byte {
	t.Helper()
	var out []byte
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		out = append(out, buf[:n]...)
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}
}
