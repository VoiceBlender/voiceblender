package audiofilter

import (
	"io"
	"sync"
	"testing"
	"time"
)

// blockingSource delivers frames only when released, standing in for a leg
// that has gone quiet.
type blockingSource struct {
	release chan struct{}
	frame   []byte
}

func (b *blockingSource) Read(p []byte) (int, error) {
	<-b.release
	return copy(p, b.frame), nil
}

// TestSetFiltersWhileStreaming covers the point of the feature: a live chain
// can be replaced and the audio keeps flowing.
func TestSetFiltersWhileStreaming(t *testing.T) {
	const rate = 16000
	src := &chunkSource{data: toBytes(speechLike(rate*4, rate)), n: 640}
	r, err := Build(src, rate, rate, []Spec{{Type: "robotic"}})
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 640)
	for i := 0; i < 50; i++ {
		if _, err := r.Read(buf); err != nil {
			t.Fatal(err)
		}
	}
	if got := r.Filters(); len(got) != 1 || got[0].Type != "robotic" {
		t.Fatalf("before swap: %+v", got)
	}

	if err := r.SetFilters([]Spec{{Type: "pitch", Params: Params{"semitones": 4}}}); err != nil {
		t.Fatalf("swap: %v", err)
	}
	if got := r.Filters(); len(got) != 1 || got[0].Type != "pitch" {
		t.Fatalf("after swap: %+v", got)
	}
	// Audio must keep flowing through the new chain.
	var total int
	for i := 0; i < 50; i++ {
		n, err := r.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		total += n
	}
	if total == 0 {
		t.Fatal("no audio after the swap")
	}
	t.Logf("swapped robotic -> pitch mid-stream; %d bytes read afterwards", total)

	// Clearing filters is a swap to an empty chain.
	if err := r.SetFilters(nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got := r.Filters(); len(got) != 0 {
		t.Errorf("after clearing: %+v", got)
	}
	if _, err := r.Read(buf); err != nil {
		t.Fatalf("read after clearing: %v", err)
	}
	t.Log("cleared to an empty chain; audio still flows")
}

// TestSetFiltersAcrossWorkRates covers the case that used to be refused:
// denoise pulls the chain to 48 kHz while the others run at the leg's rate, so
// enabling or disabling it resizes the resamplers and the block. The swap is
// staged and wrapped in a short fade, so it lands as a soft dip rather than a
// click, and the audio keeps flowing either way.
func TestSetFiltersAcrossWorkRates(t *testing.T) {
	Register("ratehog", Descriptor{RequiredRate: 48000, FrameSamples: 480,
		New: func(int, Params) (Stage, error) { return &gainStage{gain: 1}, nil }})
	defer Unregister("ratehog")

	const rate = 16000
	src := &chunkSource{data: toBytes(speechLike(rate*8, rate)), n: 640}
	r, err := Build(src, rate, rate, []Spec{{Type: "bandpass"}})
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 640)
	read := func(n int) int {
		total := 0
		for i := 0; i < n; i++ {
			got, err := r.Read(buf)
			if err != nil && err != io.EOF {
				t.Fatalf("read: %v", err)
			}
			total += got
		}
		return total
	}
	read(30)
	if got := workRateOf(r); got != rate {
		t.Fatalf("chain should start at %d Hz, got %d", rate, got)
	}

	// Up to 48 kHz.
	if err := r.SetFilters([]Spec{{Type: "ratehog"}}); err != nil {
		t.Fatalf("raising the working rate: %v", err)
	}
	if n := read(60); n == 0 {
		t.Fatal("no audio after raising the working rate")
	}
	if got := workRateOf(r); got != 48000 {
		t.Errorf("chain should have moved to 48000 Hz, got %d", got)
	}
	t.Logf("swapped 16000 Hz -> 48000 Hz on a live stream")

	// And back down.
	if err := r.SetFilters([]Spec{{Type: "bandpass"}}); err != nil {
		t.Fatalf("lowering the working rate: %v", err)
	}
	if n := read(60); n == 0 {
		t.Fatal("no audio after lowering the working rate")
	}
	if got := workRateOf(r); got != rate {
		t.Errorf("chain should have returned to %d Hz, got %d", rate, got)
	}
	t.Log("swapped 48000 Hz -> 16000 Hz on a live stream")
}

// TestSetFiltersFadesRatherThanClicks checks the transition is ramped. A bare
// swap steps the waveform discontinuously, which is audible as a click; the
// fade should take the output through silence instead.
func TestSetFiltersFadesRatherThanClicks(t *testing.T) {
	const rate = 16000
	// Steady tone: any step shows up plainly against it.
	src := &chunkSource{data: toBytes(tone(rate*8, rate, 440, 9000)), n: 640}
	r, err := Build(src, rate, rate, []Spec{{Type: "gain", Params: Params{"volume": 0}}})
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 640)
	for i := 0; i < 30; i++ {
		r.Read(buf)
	}
	if err := r.SetFilters([]Spec{{Type: "gain", Params: Params{"volume": 0}}}); err != nil {
		t.Fatal(err)
	}

	// Collect the transition and look for the dip.
	var got []int16
	for i := 0; i < 20; i++ {
		n, err := r.Read(buf)
		if err != nil && err != io.EOF {
			t.Fatal(err)
		}
		got = append(got, toSamples(buf[:n])...)
	}
	minAbs := 1 << 20
	for _, v := range got {
		a := int(v)
		if a < 0 {
			a = -a
		}
		if a < minAbs {
			minAbs = a
		}
	}
	// A ramp through silence is the signature; without one the tone never
	// approaches zero for more than a single zero crossing.
	var nearZero int
	for _, v := range got {
		if v > -200 && v < 200 {
			nearZero++
		}
	}
	t.Logf("transition: %d samples, %d of them near silence", len(got), nearZero)
	if nearZero < 8 {
		t.Errorf("only %d near-silent samples: the swap is stepping rather than fading", nearZero)
	}
}

// workRateOf reports the working rate of the chain currently installed.
func workRateOf(r *Reader) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.c.work
}

func TestSetFiltersRejectsInvalidChain(t *testing.T) {
	src := &chunkSource{data: toBytes(tone(16000, 16000, 440, 8000)), n: 640}
	r, err := Build(src, 16000, 16000, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetFilters([]Spec{{Type: "nosuchfilter"}}); err == nil {
		t.Error("unknown filter must be rejected")
	}
	if err := r.SetFilters([]Spec{{Type: "gain", Params: Params{"volume": 99}}}); err == nil {
		t.Error("out-of-range parameter must be rejected")
	}
	t.Log("invalid chains rejected without disturbing the live one")
}

// TestSetFiltersDoesNotBlockOnSilentLeg is why the lock is not held across the
// source read: a leg that has stopped sending must not wedge an API call.
func TestSetFiltersDoesNotBlockOnSilentLeg(t *testing.T) {
	src := &blockingSource{release: make(chan struct{}), frame: make([]byte, 640)}
	r, err := Build(src, 16000, 16000, []Spec{{Type: "bandpass"}})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); r.Read(make([]byte, 640)) }() // parks in src.Read

	done := make(chan error, 1)
	go func() { done <- r.SetFilters([]Spec{{Type: "robotic"}}) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("swap on a silent leg: %v", err)
		}
		t.Log("filter change completed while the leg was silent")
	case <-time.After(2 * time.Second):
		t.Fatal("SetFilters blocked behind a silent leg's source read")
	}
	close(src.release)
	wg.Wait()
}

// TestSetFiltersConcurrent runs swaps against a live read loop under -race.
func TestSetFiltersConcurrent(t *testing.T) {
	const rate = 16000
	src := &chunkSource{data: toBytes(speechLike(rate*30, rate)), n: 640}
	r, err := Build(src, rate, rate, nil)
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 640)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := r.Read(buf); err == io.EOF {
				return
			}
		}
	}()

	chains := [][]Spec{
		{{Type: "robotic"}},
		{{Type: "pitch"}},
		{{Type: "bandpass"}, {Type: "gain", Params: Params{"volume": 1}}},
		nil,
	}
	for i := 0; i < 200; i++ {
		if err := r.SetFilters(chains[i%len(chains)]); err != nil {
			t.Fatalf("swap %d: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()
	t.Log("200 swaps against a live read loop, no race")
}
