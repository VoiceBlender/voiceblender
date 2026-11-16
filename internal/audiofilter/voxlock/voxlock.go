// Package voxlock provides the "voxlock" audio filter: target-speaker
// extraction via the pure-Go github.com/VoiceBlender/voxlock-go. It keeps the
// leg's own talker and removes other voices and noise behind them. It finds
// the talker by itself: audio passes through until it has heard 3 s of
// speech, which becomes the voice it keeps.
//
// The kernel is compiled in only with -tags voxlock, on the CPU, or -tags
// cuda, which adds an NVIDIA GPU with the calls beyond its share on the CPU.
// Without either the filter is registered but never available.
package voxlock

import (
	"fmt"
	"strconv"
	"sync"

	"github.com/VoiceBlender/voiceblender/internal/audiofilter"
)

// Name is the filter's registered name.
const Name = "voxlock"

// Rates are the rates the model runs at. Above 16 kHz the chain runs it at
// 16 kHz, band-limiting the leg to 8 kHz.
var Rates = []int{8000, 16000}

func init() {
	audiofilter.Register(Name, audiofilter.Descriptor{
		Unique: true,
		// Not Corrective: answering-machine detection hears a greeting before
		// the 3 s of speech the filter waits for, so it would only pay for it.
		Rates:     Rates,
		Available: Available,
		New:       newStage,
	})
}

// Config sets up the kernel.
type Config struct {
	// Model is the path to the weights, voxlock.bin.
	Model string
	// Int8 runs the CPU's calls with int8 weights, on CPUs with AVX2.
	Int8 bool
	// FrameMs is the audio per pass, a multiple of 8 up to 80. Longer frames
	// cost less per call and add their length to the latency.
	FrameMs int
	// GPU uses an NVIDIA GPU if the build has one and the host has one.
	GPU bool
	// GPUDevice is the GPU's ordinal.
	GPUDevice int
	// GPUMaxCalls is how many calls the GPU takes; the rest run on the CPU.
	GPUMaxCalls int
}

var (
	mu sync.RWMutex
	k  *kernel
)

// Install loads the model and starts the kernel, making the filter
// available. A non-nil error must not abort startup: log it and leave the
// filter off. Without a usable GPU the kernel runs on the CPU; Backend says
// which, and why.
func Install(c Config) error {
	nk, err := newKernel(c)
	if err != nil {
		return err
	}
	mu.Lock()
	old := k
	k = nk
	mu.Unlock()
	if old != nil {
		old.Close()
	}
	return nil
}

// Shutdown releases the kernel and marks the filter unavailable. Legs still
// running it carry on, on the CPU.
func Shutdown() error {
	mu.Lock()
	old := k
	k = nil
	mu.Unlock()
	if old == nil {
		return nil
	}
	return old.Close()
}

// Stats are the kernel's counts.
type Stats struct {
	Streams    int // legs running the filter
	Instances  int // per-stream states, live plus pooled
	GPUStreams int // of Streams, those on the GPU
	GPUPasses  uint64
	GPUFailed  uint64 // passes the GPU failed, run on the CPU instead
}

// CurrentStats is zero when no kernel is installed.
func CurrentStats() Stats {
	if cur := current(); cur != nil {
		return cur.Stats()
	}
	return Stats{}
}

// Backend describes where the kernel runs, or is empty with none installed.
func Backend() string {
	if cur := current(); cur != nil {
		return cur.backend
	}
	return ""
}

// Available reports whether Install has succeeded.
func Available() bool { return current() != nil }

func current() *kernel {
	mu.RLock()
	defer mu.RUnlock()
	return k
}

type stage struct {
	st *stream
}

func newStage(rate int, _ audiofilter.Params) (audiofilter.Stage, error) {
	cur := current()
	if cur == nil {
		return nil, errUnavailable{}
	}
	st, err := cur.acquire(rate)
	if err != nil {
		return nil, err
	}
	return &stage{st: st}, nil
}

func (s *stage) FrameSamples() int {
	if s.st == nil {
		return 0
	}
	return s.st.frame
}

// Process leaves the frame untouched on error: a degraded but audible leg
// beats a dead one.
func (s *stage) Process(frame []int16) {
	if s.st == nil {
		return
	}
	_ = s.st.process(frame)
}

func (s *stage) Close() error {
	if s.st != nil {
		s.st.release()
		s.st = nil
	}
	return nil
}

type errUnavailable struct{}

func (errUnavailable) Error() string { return Name + " kernel is not installed" }

type errRate struct{ got int }

func (e errRate) Error() string {
	return Name + " runs at 8000 or 16000 Hz, chain resolved to " + strconv.Itoa(e.got)
}

func validFrameMs(ms int) error {
	if ms < 8 || ms > 80 || ms%8 != 0 {
		return fmt.Errorf("%s frame must be a multiple of 8 ms up to 80, got %d", Name, ms)
	}
	return nil
}
