package audiofilter

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
	"testing"
)

// chunkSource yields at most n bytes per Read from a fixed buffer without
// allocating, so allocation tests measure the chain and not the source.
type chunkSource struct {
	data   []byte
	n      int
	off    int
	closed bool
}

func (c *chunkSource) Read(p []byte) (int, error) {
	if c.off >= len(c.data) {
		return 0, io.EOF
	}
	n := c.n
	if n > len(p) {
		n = len(p)
	}
	if c.off+n > len(c.data) {
		n = len(c.data) - c.off
	}
	copy(p, c.data[c.off:c.off+n])
	c.off += n
	return n, nil
}

func (c *chunkSource) Close() error { c.closed = true; return nil }

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

// tone builds n samples of a sine at hz, amplitude amp, sampled at rate.
func tone(n, rate int, hz, amp float64) []int16 {
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(amp * math.Sin(2*math.Pi*hz*float64(i)/float64(rate)))
	}
	return out
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

func readAll(t *testing.T, r io.Reader, readSize int) []byte {
	t.Helper()
	var got bytes.Buffer
	buf := make([]byte, readSize)
	for {
		n, err := r.Read(buf)
		got.Write(buf[:n])
		if err == io.EOF {
			return got.Bytes()
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}
}

// TestFramingIndependence is the load-bearing test: output must be identical no
// matter how the source chunks its input or how large the consumer's buffer is.
// The chain sits between two components that frame audio differently, so any
// dependence on either framing is a bug that would surface as intermittent
// artefacts under load.
func TestFramingIndependence(t *testing.T) {
	const rate = 16000
	input := toBytes(tone(rate*2, rate, 440, 8000))
	specs := []Spec{{Type: "bandpass"}}

	variants := []struct{ srcChunk, readSize int }{
		{640, 4096},  // 20 ms frames, large reads: the realistic case
		{1, 4096},    // one byte at a time
		{3, 7},       // odd on both sides: splits samples and blocks
		{4096, 1},    // large chunks, one-byte reads
		{999, 333},   // co-prime with the block size
		{65536, 640}, // whole buffer at once
	}

	var golden []byte
	for _, v := range variants {
		src := &chunkSource{data: input, n: v.srcChunk}
		r, err := Build(src, rate, rate, specs)
		if err != nil {
			t.Fatal(err)
		}
		got := readAll(t, r, v.readSize)
		if golden == nil {
			golden = got
			if len(got) != len(input) {
				t.Fatalf("length changed: %d in, %d out", len(input), len(got))
			}
			t.Logf("reference: srcChunk=%-6d readSize=%-5d -> %d bytes", v.srcChunk, v.readSize, len(got))
			continue
		}
		if !bytes.Equal(golden, got) {
			t.Errorf("srcChunk=%d readSize=%d: differs from reference (%d vs %d bytes)",
				v.srcChunk, v.readSize, len(got), len(golden))
			continue
		}
		t.Logf("srcChunk=%-6d readSize=%-5d -> identical", v.srcChunk, v.readSize)
	}
}

// TestReadDoesNotAllocate guards the steady-state hot path: this runs per frame
// on every filtered leg, so per-Read allocation is GC pressure multiplied by
// concurrent calls.
func TestReadDoesNotAllocate(t *testing.T) {
	const rate = 16000
	data := toBytes(tone(rate*60, rate, 440, 8000))
	src := &chunkSource{data: data, n: 640}
	r, err := Build(src, rate, rate, []Spec{{Type: "bandpass"}, {Type: "gain", Params: Params{"volume": 1}}})
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 640)
	for i := 0; i < 200; i++ { // warm every buffer to its steady size
		if _, err := r.Read(buf); err != nil {
			t.Fatal(err)
		}
	}
	got := testing.AllocsPerRun(2000, func() {
		if _, err := r.Read(buf); err != nil && err != io.EOF {
			t.Fatal(err)
		}
	})
	t.Logf("steady-state allocations per Read: %.2f", got)
	if got > 0 {
		t.Errorf("Read allocates %.2f times per call in steady state, want 0", got)
	}
}

func TestResolveWorkRate(t *testing.T) {
	Register("ratetest", Descriptor{RequiredRate: 48000, FrameSamples: 480,
		New: func(int, Params) (Stage, error) { return &gainStage{gain: 1}, nil }})
	defer Unregister("ratetest")

	cases := []struct {
		list    string
		dstRate int
		want    int
	}{
		{"bandpass", 8000, 8000},
		{"gain,bandpass", 8000, 8000},
		{"ratetest", 8000, 48000},
		{"bandpass,ratetest", 16000, 48000},
	}
	for _, c := range cases {
		specs, err := Parse(c.list)
		if err != nil {
			t.Fatalf("%s: %v", c.list, err)
		}
		if got := ResolveWorkRate(specs, c.dstRate); got != c.want {
			t.Errorf("%-18s dst=%d: got %d, want %d", c.list, c.dstRate, got, c.want)
			continue
		}
		t.Logf("%-18s dst=%-6d -> work rate %d Hz", c.list, c.dstRate, ResolveWorkRate(specs, c.dstRate))
	}
}

// TestBlockSizeIsLCM checks a chain mixing frame requirements runs each stage
// over the right sub-slices.
func TestBlockSizeIsLCM(t *testing.T) {
	var calls480, calls960 int
	Register("f480", Descriptor{RequiredRate: 48000, FrameSamples: 480,
		New: func(int, Params) (Stage, error) { return &countStage{n: &calls480, want: 480}, nil }})
	Register("f960", Descriptor{RequiredRate: 48000, FrameSamples: 960,
		New: func(int, Params) (Stage, error) { return &countStage{n: &calls960, want: 960}, nil }})
	defer func() { Unregister("f480"); Unregister("f960") }()

	c, err := buildChain(48000, 48000, []Spec{{Type: "f480"}, {Type: "f960"}})
	if err != nil {
		t.Fatal(err)
	}
	if c.hop != 960 {
		t.Fatalf("block size %d, want lcm(480,960)=960", c.hop)
	}
	c.process(make([]int16, c.hop))
	if calls480 != 2 || calls960 != 1 {
		t.Errorf("per block: 480-stage called %d times (want 2), 960-stage %d (want 1)", calls480, calls960)
	}
	t.Logf("block=%d: 480-stage x%d, 960-stage x%d", c.hop, calls480, calls960)
}

type countStage struct {
	n    *int
	want int
}

func (c *countStage) Close() error { return nil }
func (c *countStage) Process(frame []int16) {
	if len(frame) != c.want {
		panic("stage got wrong frame length")
	}
	*c.n++
}

// TestEmptyChainStillBuildsAReader: an empty chain is a real Reader, not the
// plain resampling reader, so filters can be added to a leg that started with
// none. It costs nothing — an empty chain benchmarks identical to the plain
// reader and allocates less.
func TestEmptyChainStillBuildsAReader(t *testing.T) {
	in := toBytes(tone(16000, 16000, 440, 8000))
	src := &chunkSource{data: in, n: 640}
	r, err := Build(src, 16000, 16000, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := readAll(t, r, 4096)
	if len(out) != len(in) {
		t.Errorf("empty chain changed the sample count: %d in, %d out", len(in), len(out))
	}
	if !bytes.Equal(out, in) {
		t.Error("an empty chain at equal rates must pass audio through unchanged")
	}
	if got := r.Filters(); len(got) != 0 {
		t.Errorf("empty chain should report no filters, got %+v", got)
	}
	t.Log("empty chain is transparent and ready to have filters added")
}

// TestCloseReleasesStagesButNotTheSource pins a distinction the mixer depends
// on. It closes a participant's reader on teardown, which includes a leg moving
// rooms — so closing the leg's own source here would leave a pipe-backed leg
// (a LiveKit participant hands over an io.PipeReader) silent in its new room.
// The stages must still be released, or a denoise state leaks on every move.
func TestCloseReleasesStagesButNotTheSource(t *testing.T) {
	src := &chunkSource{data: toBytes(tone(1600, 16000, 440, 8000)), n: 640}
	var closed bool
	Register("closespy", Descriptor{
		New: func(int, Params) (Stage, error) { return &spyStage{closed: &closed}, nil },
	})
	defer Unregister("closespy")

	r, err := Build(src, 16000, 16000, []Spec{{Type: "closespy"}})
	if err != nil {
		t.Fatal(err)
	}
	readAll(t, r, 640)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if !closed {
		t.Error("Close must release the stages, or a denoise state leaks on every room move")
	}
	if src.closed {
		t.Error("Close must not close the leg's source: the same reader is handed to the next room")
	}
	t.Log("stages released, source left open for the next room")
}

type spyStage struct{ closed *bool }

func (s *spyStage) Process([]int16) {}
func (s *spyStage) Close() error    { *s.closed = true; return nil }

// TestRateConversion checks the chain converts rates correctly while filtering.
func TestRateConversion(t *testing.T) {
	for _, c := range []struct{ src, dst int }{{8000, 16000}, {16000, 8000}, {16000, 16000}} {
		in := tone(c.src*2, c.src, 440, 8000)
		src := &chunkSource{data: toBytes(in), n: c.src / 50 * 2}
		r, err := Build(src, c.src, c.dst, []Spec{{Type: "bandpass"}})
		if err != nil {
			t.Fatal(err)
		}
		out := toSamples(readAll(t, r, 4096))
		want := len(in) * c.dst / c.src
		if d := len(out) - want; d < -c.dst/100 || d > c.dst/100 {
			t.Errorf("%d->%d: got %d samples, want ~%d", c.src, c.dst, len(out), want)
			continue
		}
		t.Logf("%5d -> %-5d Hz: %d samples in, %d out (want ~%d)", c.src, c.dst, len(in), len(out), want)
	}
}
