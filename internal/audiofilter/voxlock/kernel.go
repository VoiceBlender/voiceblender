//go:build voxlock || cuda

package voxlock

import (
	"fmt"
	"sync"

	vl "github.com/VoiceBlender/voxlock-go"
)

// Compiled reports whether the build carries the kernel.
const Compiled = true

// kernel holds the shared read-only model, a pool of per-stream states keyed
// by rate, and the GPU if there is one.
type kernel struct {
	model   *vl.Model
	frames  int
	backend string

	gpu      *gpu // nil on the CPU alone
	gpuCalls int

	mu     sync.Mutex
	free   map[int][]*vl.LiveStream
	live   int
	built  int
	onGPU  int
	closed bool
}

type gpu struct {
	close    func() error
	batchers map[int]*vl.Batcher
}

func newKernel(c Config) (*kernel, error) {
	if c.Model == "" {
		return nil, fmt.Errorf("%s needs a model file", Name)
	}
	if err := validFrameMs(c.FrameMs); err != nil {
		return nil, err
	}
	m, err := vl.LoadModelFile(c.Model)
	if err != nil {
		return nil, fmt.Errorf("load voxlock weights: %w", err)
	}
	cpu := "CPU fp32"
	if c.Int8 && m.Quantize() {
		cpu = "CPU int8"
	}
	k := &kernel{model: m, frames: c.FrameMs / 8, free: map[int][]*vl.LiveStream{}, backend: cpu}
	probe, err := k.newStream(16000)
	if err != nil {
		return nil, fmt.Errorf("create stream: %w", err)
	}
	k.free[16000], k.built = []*vl.LiveStream{probe}, 1
	if c.GPU && c.GPUMaxCalls > 0 {
		if err := k.openGPU(c); err != nil {
			k.backend = cpu + "; GPU unavailable: " + err.Error()
		}
	}
	return k, nil
}

func (k *kernel) openGPU(c Config) error {
	dev, name, closeDev, err := openDevice(c.GPUDevice, c.GPUMaxCalls)
	if err != nil {
		return err
	}
	g := &gpu{close: closeDev, batchers: map[int]*vl.Batcher{}}
	for _, rate := range Rates {
		o := vl.Options{Model: k.model, SampleRate: rate, Frames: k.frames}
		// Two workers: one gathers while the other's pass runs; more only
		// split the GPU's passes smaller.
		b, err := vl.NewBatcher(dev, o, c.GPUMaxCalls, 2)
		if err != nil {
			g.Close()
			return err
		}
		g.batchers[rate] = b
	}
	k.gpu, k.gpuCalls = g, c.GPUMaxCalls
	k.backend = fmt.Sprintf("GPU %s for up to %d calls, then %s", name, c.GPUMaxCalls, k.backend)
	return nil
}

func (g *gpu) Close() error {
	for _, b := range g.batchers {
		b.Close()
	}
	return g.close()
}

func (k *kernel) newStream(rate int) (*vl.LiveStream, error) {
	return vl.NewLiveStream(vl.Options{Model: k.model, SampleRate: rate, Frames: k.frames, Background: true})
}

func (k *kernel) Close() error {
	k.mu.Lock()
	k.closed = true
	k.free = nil
	g := k.gpu
	k.mu.Unlock()
	if g != nil {
		return g.Close()
	}
	return nil
}

func (k *kernel) Stats() Stats {
	k.mu.Lock()
	s := Stats{Streams: k.live, Instances: k.built, GPUStreams: k.onGPU}
	k.mu.Unlock()
	if k.gpu != nil {
		for _, b := range k.gpu.batchers {
			st := b.Stats()
			s.GPUPasses += st.Passes
			s.GPUFailed += st.Failed
		}
	}
	return s
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
	var l *vl.LiveStream
	if pool := k.free[rate]; len(pool) > 0 {
		l = pool[len(pool)-1]
		k.free[rate] = pool[:len(pool)-1]
	}
	var b *vl.Batcher
	if k.gpu != nil && k.onGPU < k.gpuCalls {
		b = k.gpu.batchers[rate]
		k.onGPU++
	}
	k.live++
	k.mu.Unlock()
	if l == nil {
		var err error
		if l, err = k.newStream(rate); err != nil {
			k.mu.Lock()
			k.live--
			if b != nil {
				k.onGPU--
			}
			k.mu.Unlock()
			return nil, err
		}
		k.mu.Lock()
		k.built++
		k.mu.Unlock()
	}
	return &stream{k: k, l: l, b: b, rate: rate, frame: l.FrameSize()}, nil
}

// stream is one leg's state, driven from that leg's audio goroutine only.
type stream struct {
	k     *kernel
	l     *vl.LiveStream
	b     *vl.Batcher // nil on the CPU
	rate  int
	frame int
}

func (s *stream) process(frame []int16) error {
	if s.l == nil {
		return fmt.Errorf("state released")
	}
	if len(frame) != s.frame {
		return fmt.Errorf("frame must be %d samples at %d Hz, got %d", s.frame, s.rate, len(frame))
	}
	if s.b != nil {
		return s.b.ProcessInt16(s.l, frame, frame)
	}
	return s.l.ProcessInt16(frame, frame)
}

// release returns the state to its kernel. Every acquired stream must reach
// here.
func (s *stream) release() {
	if s.l == nil {
		return
	}
	l := s.l
	s.l = nil
	// Forgets the voice, and frees its place on the GPU, now rather than at
	// the next call.
	l.Reset()
	k := s.k
	k.mu.Lock()
	k.live--
	if s.b != nil {
		k.onGPU--
	}
	if !k.closed {
		k.free[s.rate] = append(k.free[s.rate], l)
	}
	k.mu.Unlock()
}
