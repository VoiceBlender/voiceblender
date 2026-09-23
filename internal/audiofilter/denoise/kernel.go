package denoise

import (
	"fmt"
	"sync"

	rnnoise "github.com/VoiceBlender/rnnoise-go"
	rnmodel "github.com/VoiceBlender/rnnoise-go/model"
)

// MinRate is the lowest rate the model is defined for. Below it the denoiser
// would be extrapolating past anything it was trained on.
const MinRate = rnnoise.MinSampleRate

// ReferenceRate is the rate upstream's constants are defined at. The filter
// does not require it: a stream is denoised at whatever rate the chain already
// runs, so no resampling is added for the filter's sake.
const ReferenceRate = rnnoise.DefaultSampleRate

// kernel holds the model and a pool of per-stream states.
//
// The model is read-only, so one copy of the weights backs every stream on the
// server and a frame is denoised in place. What is worth pooling is the state
// itself: a Denoiser allocates all of its scratch up front and none afterwards,
// so recycling one across calls avoids re-allocating about 60 KB per leg on a
// system that churns legs continuously.
//
// The pool is keyed by rate because a Denoiser's tables are rate-specific, and
// legs run at whatever rate their chain resolved to.
type kernel struct {
	model *rnnoise.Model

	mu     sync.Mutex
	free   map[int][]*rnnoise.Denoiser
	live   int
	built  int
	closed bool
}

func newKernel() (*kernel, error) {
	m, err := rnmodel.Load()
	if err != nil {
		return nil, fmt.Errorf("load rnnoise weights: %w", err)
	}
	// Fail here rather than at the first call if the weights cannot drive a
	// denoiser at all.
	probe, err := rnnoise.New(rnnoise.Options{SampleRate: ReferenceRate, Model: m})
	if err != nil {
		return nil, fmt.Errorf("create denoiser: %w", err)
	}
	return &kernel{
		model: m,
		free:  map[int][]*rnnoise.Denoiser{ReferenceRate: {probe}},
		built: 1,
	}, nil
}

func (k *kernel) Close() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.closed = true
	k.free = nil
	return nil
}

// Stats reports live streams and the states this kernel has built, live plus
// pooled.
func (k *kernel) Stats() (streams, states int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.live, k.built
}

func (k *kernel) acquire(rate int) (*stream, error) {
	if rate < MinRate {
		return nil, errRate{got: rate}
	}
	k.mu.Lock()
	if k.closed {
		k.mu.Unlock()
		return nil, fmt.Errorf("denoise kernel is closed")
	}
	var d *rnnoise.Denoiser
	if pool := k.free[rate]; len(pool) > 0 {
		d = pool[len(pool)-1]
		k.free[rate] = pool[:len(pool)-1]
		// A recycled Denoiser carries the previous call's recurrent state, so
		// it must be cleared before it hears a different voice.
		d.Reset()
	}
	k.live++
	k.mu.Unlock()

	if d == nil {
		var err error
		d, err = rnnoise.New(rnnoise.Options{SampleRate: rate, Model: k.model})
		if err != nil {
			k.mu.Lock()
			k.live--
			k.mu.Unlock()
			return nil, err
		}
		k.mu.Lock()
		k.built++
		k.mu.Unlock()
	}
	return &stream{k: k, d: d, rate: rate, frame: d.FrameSize()}, nil
}

// stream is one leg's state. A Denoiser is not safe for concurrent use, but
// each leg has its own and the audio path calls process from one goroutine, so
// no lock is needed here.
type stream struct {
	k     *kernel
	d     *rnnoise.Denoiser
	rate  int
	frame int
}

func (s *stream) process(frame []int16) error {
	if s.d == nil {
		return fmt.Errorf("state released")
	}
	if len(frame) != s.frame {
		return fmt.Errorf("frame must be %d samples at %d Hz, got %d", s.frame, s.rate, len(frame))
	}
	// ProcessInt16 works in place and in int16 magnitude, which is what a PCM
	// frame already carries, so there is no scaling at the boundary.
	_, err := s.d.ProcessInt16(frame, frame)
	return err
}

// release returns the state to its kernel. Every acquired stream must reach
// here.
func (s *stream) release() {
	if s.d == nil {
		return
	}
	d := s.d
	s.d = nil
	k := s.k
	k.mu.Lock()
	k.live--
	if !k.closed {
		k.free[s.rate] = append(k.free[s.rate], d)
	}
	k.mu.Unlock()
}
