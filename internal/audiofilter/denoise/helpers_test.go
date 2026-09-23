package denoise

import (
	"encoding/binary"
	"io"
	"testing"
)

// Rate and Hop are the reference rate and its 10 ms frame. The filter is not
// pinned to them -- see TestNativeRates -- but the signal-quality tests fix a
// rate so their numbers stay comparable.
const (
	Rate = ReferenceRate
	Hop  = ReferenceRate / 100
)

type sliceReader struct {
	data []byte
	n    int
	off  int
}

func (s *sliceReader) Read(p []byte) (int, error) {
	if s.off >= len(s.data) {
		return 0, io.EOF
	}
	n := s.n
	if n > len(p) {
		n = len(p)
	}
	if s.off+n > len(s.data) {
		n = len(s.data) - s.off
	}
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
