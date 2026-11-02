//go:build cuda

package voxlock

import (
	"strings"
	"testing"

	"github.com/VoiceBlender/voxlock-go/cuda"
)

func gpuConfig(t *testing.T, calls int) Config {
	if cuda.Devices() == 0 {
		t.Skip("no CUDA GPU")
	}
	c := testConfig(t)
	c.GPU, c.GPUMaxCalls = true, calls
	return c
}

// TestGPU: calls up to GPUMaxCalls go to the GPU, the rest to the CPU, and
// the GPU's isolate as the CPU's do.
func TestGPU(t *testing.T) {
	install(t, gpuConfig(t, 2))
	if !strings.HasPrefix(Backend(), "GPU ") {
		t.Fatalf("backend %q", Backend())
	}
	t.Logf("backend: %s", Backend())
	k := current()
	var ss []*stream
	for range 3 {
		st, err := k.acquire(16000)
		if err != nil {
			t.Fatal(err)
		}
		ss = append(ss, st)
	}
	if ss[0].b == nil || ss[1].b == nil || ss[2].b != nil {
		t.Fatal("want the first two calls on the GPU and the third on the CPU")
	}
	if got := CurrentStats().GPUStreams; got != 2 {
		t.Errorf("%d GPU streams, want 2", got)
	}
	in, out := isolation(t, ss[0])
	t.Logf("on the GPU, talker over bystanders: %+.1f dB in, %+.1f dB out", in, out)
	if out-in < 10 {
		t.Errorf("bystanders suppressed by %.1f dB, want at least 10", out-in)
	}
	if s := CurrentStats(); s.GPUPasses == 0 || s.GPUFailed != 0 {
		t.Errorf("stats %+v", s)
	}
	for _, st := range ss {
		st.release()
	}
	if s := CurrentStats(); s.GPUStreams != 0 || s.Streams != 0 {
		t.Errorf("after release: %+v", s)
	}
	// A call after the others left takes the GPU again.
	st, _ := k.acquire(8000)
	defer st.release()
	if st.b == nil {
		t.Error("a freed GPU place was not reused")
	}
}

// TestShutdownOnGPU: a call on the GPU when the kernel goes carries on, on
// the CPU.
func TestShutdownOnGPU(t *testing.T) {
	install(t, gpuConfig(t, 4))
	st, err := current().acquire(16000)
	if err != nil {
		t.Fatal(err)
	}
	defer st.release()
	a := talker(t)
	runFrames(t, st, a[:8*16000], true)
	if !st.l.Locked() {
		t.Fatal("never locked")
	}
	Shutdown()
	out := runFrames(t, st, a[8*16000:], false)
	if energy(out) == 0 {
		t.Error("no audio after Shutdown")
	}
}
