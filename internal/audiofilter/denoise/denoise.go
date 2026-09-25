// Package denoise provides the "denoise" audio filter: RNNoise noise
// suppression, as a pure-Go port of upstream's current model
// (github.com/VoiceBlender/rnnoise-go). It needs no cgo, so the static
// CGO_ENABLED=0 build is preserved.
package denoise

import (
	"sync"

	"github.com/VoiceBlender/voiceblender/internal/audiofilter"
)

// The filter registers itself on import so its name is always known to
// validation and to generated documentation, whether or not the kernel starts.
// Availability then follows Install: a failed kernel leaves the name valid and
// the filter unavailable, which audiofilter.Resolve turns into a passthrough
// rather than a failed call.
func init() {
	// No RequiredRate: the model runs natively at any rate from MinRate up, so
	// a leg is denoised at the rate its chain already resolved to. Pinning the
	// filter to 48 kHz would have every narrowband call resampled up and back
	// down for the filter's sake alone, which costs latency and CPU and
	// recovers nothing -- there is no content above the leg's own Nyquist to
	// recover. The stage reports its own frame length, which follows the rate.
	audiofilter.Register("denoise", audiofilter.Descriptor{
		Unique:     true,
		Corrective: true,
		Available:  Available,
		New:        newStage,
	})
}

var (
	mu sync.RWMutex
	k  *kernel
)

// Install starts the kernel and makes the filter available. A non-nil error
// must not abort startup: log it and leave denoising off, because refusing to
// place calls over an unavailable filter is worse than placing them unfiltered.
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

// Stats reports live denoise streams and the per-stream states the kernel
// keeps. Zero when no kernel is installed.
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

// FrameSamples is the 10 ms frame the denoiser consumes at the rate this stage
// was built for. The chain reads it to size its block.
func (s *stage) FrameSamples() int {
	if s.st == nil {
		return 0
	}
	return s.st.frame
}

// Process leaves the frame untouched on error rather than emitting silence: a
// degraded but audible leg beats a dead one.
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

func (errUnavailable) Error() string { return "denoise kernel is not installed" }

type errRate struct{ got int }

func (e errRate) Error() string {
	return "denoise requires at least " + itoa(MinRate) + " Hz, chain resolved to " + itoa(e.got)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
