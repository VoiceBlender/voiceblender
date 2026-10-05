package denoisegtcrn

import (
	"fmt"
	"sync"

	gtcrn "github.com/VoiceBlender/gtcrn-go"
)

// kernel holds the shared read-only model and a pool of per-stream states,
// keyed by rate because a Denoiser's tables are rate-specific.
type kernel struct {
	model *gtcrn.Model

	mu     sync.Mutex
	free   map[int][]*gtcrn.Denoiser
	live   int
	built  int
	closed bool
}

func newKernel() (*kernel, error) {
	m, err := gtcrn.DefaultModel()
	if err != nil {
		return nil, fmt.Errorf("load gtcrn weights: %w", err)
	}
	probe, err := gtcrn.New(gtcrn.Options{SampleRate: 16000, Model: m})
	if err != nil {
		return nil, fmt.Errorf("create denoiser: %w", err)
	}
	return &kernel{
		model: m,
		free:  map[int][]*gtcrn.Denoiser{16000: {probe}},
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

func supported(rate int) bool {
	for _, r := range Rates {
		if r == rate {
			return true
		}
	}
	return false
}

func (k *kernel) acquire(rate int) (*stream, error) {
	if !supported(rate) {
		return nil, errRate{got: rate}
	}
	k.mu.Lock()
	if k.closed {
		k.mu.Unlock()
		return nil, fmt.Errorf("%s kernel is closed", Name)
	}
	var d *gtcrn.Denoiser
	if pool := k.free[rate]; len(pool) > 0 {
		d = pool[len(pool)-1]
		k.free[rate] = pool[:len(pool)-1]
		// A recycled Denoiser carries the previous call's recurrent state.
		d.Reset()
	}
	k.live++
	k.mu.Unlock()

	if d == nil {
		var err error
		d, err = gtcrn.New(gtcrn.Options{SampleRate: rate, Model: k.model})
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

// stream is one leg's state, driven from that leg's audio goroutine only.
type stream struct {
	k     *kernel
	d     *gtcrn.Denoiser
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
	return s.d.ProcessInt16(frame, frame)
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
