package voxlock

import (
	"encoding/binary"
	"io"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// modelPath is the weights for the tests that need them, which are skipped
// without VOXLOCK_MODEL.
func modelPath(t testing.TB) string {
	t.Helper()
	p := os.Getenv("VOXLOCK_MODEL")
	if p == "" {
		t.Skip("set VOXLOCK_MODEL to voxlock.bin to run")
	}
	return p
}

func testConfig(t testing.TB) Config {
	return Config{Model: modelPath(t), Int8: true, FrameMs: 16}
}

func install(t testing.TB, c Config) {
	t.Helper()
	if err := Install(c); err != nil {
		t.Fatalf("Install: %v", err)
	}
	t.Cleanup(func() { Shutdown() })
}

// readWav returns channel ch of a 16-bit PCM WAV file.
func readWav(t testing.TB, path string, ch int) (samples []int16, rate int) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var chans int
	for off := 12; off+8 <= len(b); {
		id, n := string(b[off:off+4]), int(binary.LittleEndian.Uint32(b[off+4:]))
		body := b[off+8 : min(off+8+n, len(b))]
		switch id {
		case "fmt ":
			chans, rate = int(binary.LittleEndian.Uint16(body[2:])), int(binary.LittleEndian.Uint32(body[4:]))
		case "data":
			for i := 2 * ch; i+1 < len(body); i += 2 * chans {
				samples = append(samples, int16(binary.LittleEndian.Uint16(body[i:])))
			}
			return samples, rate
		}
		off += 8 + n + n%2
	}
	t.Fatalf("%s: no data chunk", path)
	return nil, 0
}

// talker is the 14 s of one person's speech the tests isolate.
func talker(t testing.TB) []int16 {
	s, rate := readWav(t, "../../../tests/data/static_greetings/20260401_122737_8ed1a36a.wav", 0)
	if rate != 16000 {
		t.Fatalf("talker at %d Hz", rate)
	}
	return s[:14*rate]
}

// bystanders is other people's greetings, back to back, n samples at 16 kHz.
func bystanders(t testing.TB, n int) []int16 {
	files, _ := filepath.Glob("../../../tests/data/greetings/human/*.wav")
	sort.Strings(files)
	var out []int16
	for _, f := range files {
		s, rate := readWav(t, f, 0)
		if rate != 16000 {
			continue
		}
		out = append(out, s...)
		if len(out) >= n {
			return out[:n]
		}
	}
	t.Fatalf("only %d samples of bystanders", len(out))
	return nil
}

func noise(n int, seed int64, amp float64) []int16 {
	rng := rand.New(rand.NewSource(seed))
	out := make([]int16, n)
	for i := range out {
		out[i] = clip(rng.NormFloat64() * amp)
	}
	return out
}

func clip(v float64) int16 {
	return int16(max(-32768, min(32767, v)))
}

// project fits out ≈ a·x + b·y by least squares.
func project(out, x, y []int16) (a, b float64) {
	var xx, yy, xy, ox, oy float64
	for i := range out {
		X, Y, O := float64(x[i]), float64(y[i]), float64(out[i])
		xx, yy, xy, ox, oy = xx+X*X, yy+Y*Y, xy+X*Y, ox+O*X, oy+O*Y
	}
	det := xx*yy - xy*xy
	return (ox*yy - oy*xy) / det, (oy*xx - ox*xy) / det
}

func energy(s []int16) float64 {
	e := 0.0
	for _, v := range s {
		e += float64(v) * float64(v)
	}
	return e
}

func db(x float64) float64 { return 10 * math.Log10(x) }

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

func readAll(t *testing.T, r io.Reader) []int16 {
	t.Helper()
	var out []byte
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		out = append(out, buf[:n]...)
		if err == io.EOF {
			s := make([]int16, len(out)/2)
			for i := range s {
				s[i] = int16(binary.LittleEndian.Uint16(out[i*2:]))
			}
			return s
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}
}
