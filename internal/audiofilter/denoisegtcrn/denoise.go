// Package denoisegtcrn provides the "denoise_gtcrn" audio filter: GTCRN noise
// suppression via the pure-Go github.com/VoiceBlender/gtcrn-go, an alternative
// to the RNNoise-backed "denoise" filter. It needs no cgo.
package denoisegtcrn

import (
	"strconv"
	"sync"

	"github.com/VoiceBlender/voiceblender/internal/audiofilter"
)

// Name is the filter's registered name.
const Name = "denoise_gtcrn"

// Rates are the only rates the model runs at. A chain resolving to any other
// rate is moved to the closest of these by the chain itself, so the stage never
// resamples; above 16 kHz that band-limits the leg to 8 kHz.
var Rates = []int{8000, 12000, 16000}

func init() {
	audiofilter.Register(Name, audiofilter.Descriptor{
		Unique:     true,
		Corrective: true,
		Group:      "denoise",
		Rates:      Rates,
		Available:  Available,
		New:        newStage,
	})
}

var (
	mu sync.RWMutex
	k  *kernel
)

// Install starts the kernel and makes the filter available. A non-nil error
// must not abort startup: log it and leave the filter off.
func Install() error {
	nk, err := newKernel()
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

// Shutdown releases the kernel and marks the filter unavailable.
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

// Stats reports live streams and the per-stream states the kernel keeps. Zero
// when no kernel is installed.
func Stats() (streams, states int) {
	if cur := current(); cur != nil {
		return cur.Stats()
	}
	return 0, 0
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

// FrameSamples is the model's 16 ms hop at the rate this stage was built for.
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
	return Name + " runs at 8000, 12000 or 16000 Hz, chain resolved to " + strconv.Itoa(e.got)
}
