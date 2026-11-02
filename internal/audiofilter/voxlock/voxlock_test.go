package voxlock

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/audiofilter"
	_ "github.com/VoiceBlender/voiceblender/internal/audiofilter/denoise"
)

func TestLifecycle(t *testing.T) {
	if Available() {
		t.Fatal("filter should be unavailable before Install")
	}
	install(t, testConfig(t))
	if !Available() || Backend() == "" {
		t.Fatalf("after Install: available %v, backend %q", Available(), Backend())
	}
	t.Logf("backend: %s", Backend())
	if err := Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if Available() {
		t.Error("filter should be unavailable after Shutdown")
	}
	if err := Shutdown(); err != nil {
		t.Errorf("Shutdown must be idempotent, got %v", err)
	}
}

// TestDegradedWithoutKernel: with no kernel the name still validates, Resolve
// drops it, and the chain builds as a passthrough instead of failing setup.
func TestDegradedWithoutKernel(t *testing.T) {
	if Available() {
		t.Fatal("expected no kernel installed")
	}
	specs := []audiofilter.Spec{{Type: Name}}
	if err := audiofilter.Validate(specs); err != nil {
		t.Fatalf("%s must stay a known filter name: %v", Name, err)
	}
	if resolved := audiofilter.Resolve(specs); len(resolved) != 0 {
		t.Fatalf("unavailable filter should be dropped, got %+v", resolved)
	}
	if _, err := audiofilter.Build(nil, 16000, 16000, specs); err == nil {
		t.Error("building an unavailable filter should fail explicitly")
	}
	if err := audiofilter.Validate([]audiofilter.Spec{{Type: Name}, {Type: Name}}); err == nil {
		t.Errorf("%s twice must be rejected", Name)
	}
	if err := audiofilter.Validate([]audiofilter.Spec{{Type: "denoise"}, {Type: Name}}); err != nil {
		t.Errorf("denoise,%s: %v", Name, err)
	}
}

func TestInstallRejects(t *testing.T) {
	for _, c := range []Config{
		{FrameMs: 16},
		{Model: "/nonexistent/voxlock.bin", FrameMs: 16},
		{Model: "x", FrameMs: 0},
		{Model: "x", FrameMs: 12},
		{Model: "x", FrameMs: 88},
	} {
		if err := Install(c); err == nil {
			Shutdown()
			t.Errorf("Install(%+v) should fail", c)
		}
	}
	if Available() {
		t.Error("a failed Install must leave the filter unavailable")
	}
}

// TestNoGPUFallsBack: asking for a GPU the build or host lacks leaves the
// kernel on the CPU, saying why.
func TestNoGPUFallsBack(t *testing.T) {
	c := testConfig(t)
	c.GPU, c.GPUDevice, c.GPUMaxCalls = true, 99, 4
	install(t, c)
	if !strings.Contains(Backend(), "GPU unavailable") {
		t.Errorf("backend %q", Backend())
	}
	st, err := current().acquire(16000)
	if err != nil {
		t.Fatal(err)
	}
	defer st.release()
	if st.b != nil || CurrentStats().GPUStreams != 0 {
		t.Error("a stream went to a GPU that is not there")
	}
}

func TestFrameSizes(t *testing.T) {
	for _, ms := range []int{16, 40} {
		c := testConfig(t)
		c.FrameMs = ms
		install(t, c)
		for _, rate := range Rates {
			st, err := current().acquire(rate)
			if err != nil {
				t.Fatalf("%d Hz: %v", rate, err)
			}
			s := &stage{st: st}
			if got, want := s.FrameSamples(), rate*ms/1000; got != want {
				t.Errorf("%d ms at %d Hz: frame %d, want %d", ms, rate, got, want)
			}
			s.Close()
		}
		Shutdown()
	}
}

func TestRejectsUnsupportedRate(t *testing.T) {
	install(t, testConfig(t))
	for _, rate := range []int{12000, 24000, 48000} {
		if _, err := newStage(rate, nil); err == nil {
			t.Errorf("%d Hz should be rejected", rate)
		}
	}
}

// TestPassesThroughUntilLocked: without speech the leg is its own audio,
// delayed by the model's 24 ms.
func TestPassesThroughUntilLocked(t *testing.T) {
	install(t, testConfig(t))
	st, err := current().acquire(16000)
	if err != nil {
		t.Fatal(err)
	}
	defer st.release()
	in := noise(16000*2, 3, 2000)
	out := runFrames(t, st, in, false)
	const d = 384
	for i := d; i < len(out); i++ {
		if out[i] != in[i-d] {
			t.Fatalf("sample %d is %d, input %d ms before was %d", i, out[i], d/16, in[i-d])
		}
	}
}

// runFrames runs in through st. With pace, it waits a frame's time per frame
// while the voice is being taken, as a call would.
func runFrames(t *testing.T, st *stream, in []int16, pace bool) []int16 {
	t.Helper()
	buf := append([]int16(nil), in...)
	for off := 0; off+st.frame <= len(buf); off += st.frame {
		if err := st.process(buf[off : off+st.frame]); err != nil {
			t.Fatal(err)
		}
		if pace && off > 3*st.rate && !st.l.Locked() {
			time.Sleep(time.Duration(st.frame) * time.Second / time.Duration(st.rate))
		}
	}
	return buf
}

// isolation runs a talker who speaks alone for 5 s, then over bystanders,
// and returns the talker's level over theirs in and out, after the lock.
func isolation(t *testing.T, st *stream) (in, out float64) {
	a := talker(t)
	b := bystanders(t, len(a))
	// The bystanders start at 5 s, 3 dB under the talker.
	scale := 0.7 * sqrt(energy(a[5*16000:])/energy(b[:len(a)-5*16000]))
	mix := make([]int16, len(a))
	by := make([]int16, len(a))
	for i := range mix {
		if i >= 5*16000 {
			by[i] = clip(float64(b[i-5*16000]) * scale)
		}
		mix[i] = clip(float64(a[i]) + float64(by[i]))
	}
	got := runFrames(t, st, mix, true)
	if !st.l.Locked() {
		t.Fatal("never locked on to the talker")
	}
	const d, w0 = 384, 6 * 16000
	ga, gb := project(got[w0+d:], a[w0:len(a)-d], by[w0:len(a)-d])
	ea, eb := energy(a[w0:len(a)-d]), energy(by[w0:len(a)-d])
	return db(ea / eb), db(ga * ga * ea / (gb * gb * eb))
}

func sqrt(x float64) float64 {
	z := x
	for range 50 {
		z = (z + x/z) / 2
	}
	return z
}

// TestIsolatesTalker: the bystanders drop well below the talker.
func TestIsolatesTalker(t *testing.T) {
	install(t, testConfig(t))
	st, err := current().acquire(16000)
	if err != nil {
		t.Fatal(err)
	}
	defer st.release()
	in, out := isolation(t, st)
	t.Logf("talker over bystanders: %+.1f dB in, %+.1f dB out", in, out)
	if out-in < 10 {
		t.Errorf("bystanders suppressed by %.1f dB, want at least 10", out-in)
	}
}

// TestRecycledStateForgetsVoice: a state back from the pool passes through
// again, rather than keeping the last caller's voice.
func TestRecycledStateForgetsVoice(t *testing.T) {
	install(t, testConfig(t))
	k := current()
	st, err := k.acquire(16000)
	if err != nil {
		t.Fatal(err)
	}
	runFrames(t, st, talker(t)[:8*16000], true)
	if !st.l.Locked() {
		t.Fatal("never locked")
	}
	st.release()
	if live := k.Stats().Streams; live != 0 {
		t.Fatalf("%d live after release", live)
	}
	re, err := k.acquire(16000)
	if err != nil {
		t.Fatal(err)
	}
	defer re.release()
	if re.l.Locked() {
		t.Fatal("recycled state kept its voice")
	}
	in := noise(256*60, 5, 2000)
	out := runFrames(t, re, in, false)
	for i := 384; i < len(out); i++ {
		if out[i] != in[i-384] {
			t.Fatalf("recycled state is not a pass-through at sample %d", i)
		}
	}
}

func TestConcurrentStates(t *testing.T) {
	install(t, testConfig(t))
	k := current()
	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		s, err := k.acquire(Rates[i%len(Rates)])
		if err != nil {
			t.Fatal(err)
		}
		defer s.release()
		wg.Add(1)
		go func() {
			defer wg.Done()
			frame := make([]int16, s.frame)
			buf := noise(s.frame, int64(i), 3000)
			for range 50 {
				copy(frame, buf)
				if err := s.process(frame); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent process: %v", err)
	}
	if got := k.Stats().Streams; got != n {
		t.Errorf("%d live streams, want %d", got, n)
	}
}

// TestThroughChain: at 8 and 16 kHz the filter runs at the room rate; in a
// 48 kHz room the chain runs it at 16 kHz.
func TestThroughChain(t *testing.T) {
	install(t, testConfig(t))
	for rate, work := range map[int]int{8000: 8000, 16000: 16000, 48000: 16000} {
		specs := audiofilter.Resolve([]audiofilter.Spec{{Type: Name}})
		if got := audiofilter.ResolveWorkRate(specs, rate); got != work {
			t.Fatalf("%d Hz room: chain at %d Hz, want %d", rate, got, work)
		}
		in := noise(rate, 13, 2000)
		r, err := audiofilter.Build(&sliceReader{data: toBytes(in), n: rate / 50 * 2}, rate, rate, specs)
		if err != nil {
			t.Fatal(err)
		}
		out := readAll(t, r)
		r.Close()
		if d := len(out) - len(in); d < -rate/50 || d > rate/50 {
			t.Errorf("%d Hz: %d samples in, %d out", rate, len(in), len(out))
		}
	}
	if live := current().Stats().Streams; live != 0 {
		t.Errorf("Close did not return %d states to the pool", live)
	}
}
