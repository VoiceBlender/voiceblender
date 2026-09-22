package denoise

import (
	"context"
	"sync"

	"github.com/VoiceBlender/voiceblender/internal/audiofilter"
)

// The filter registers itself on import so its name is always known to
// validation and to generated documentation, whether or not the kernel starts.
// Availability then follows Install: a failed kernel leaves the name valid and
// the filter unavailable, which audiofilter.Resolve turns into a passthrough
// rather than a failed call.
func init() {
	audiofilter.Register("denoise", audiofilter.Descriptor{
		RequiredRate: Rate,
		FrameSamples: Hop,
		Unique:       true,
		Corrective:   true,
		Available:    Available,
		New:          newStage,
	})
}

var (
	mu     sync.RWMutex
	kernel *Kernel
)

// Install compiles the kernel and makes the filter available. A non-nil error
// must not abort startup: log it and leave denoising off, because refusing to
// place calls over an unavailable filter is worse than placing them unfiltered.
func Install(ctx context.Context, statesPerInstance int) error {
	k, err := NewKernel(ctx, statesPerInstance)
	if err != nil {
		return err
	}
	mu.Lock()
	old := kernel
	kernel = k
	mu.Unlock()
	if old != nil {
		old.Close()
	}
	return nil
}

// Shutdown releases the kernel and marks the filter unavailable.
func Shutdown() error {
	mu.Lock()
	k := kernel
	kernel = nil
	mu.Unlock()
	if k == nil {
		return nil
	}
	return k.Close()
}

// Stats reports live denoise streams and pooled wasm instances. Zero when no
// kernel is installed.
func Stats() (streams, instances int) {
	k := current()
	if k == nil {
		return 0, 0
	}
	return k.Stats()
}

// Available reports whether Install has succeeded.
func Available() bool {
	mu.RLock()
	defer mu.RUnlock()
	return kernel != nil
}

func current() *Kernel {
	mu.RLock()
	defer mu.RUnlock()
	return kernel
}

type stage struct {
	st *state
}

func newStage(rate int, _ audiofilter.Params) (audiofilter.Stage, error) {
	k := current()
	if k == nil {
		return nil, errUnavailable{}
	}
	if rate != Rate {
		return nil, errRate{got: rate}
	}
	st, err := k.acquire()
	if err != nil {
		return nil, err
	}
	return &stage{st: st}, nil
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
	return "denoise requires 48000 Hz, chain resolved to " + itoa(e.got)
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
