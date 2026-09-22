// Package denoise provides the "denoise" audio filter: RNNoise running as
// WebAssembly under wazero, so noise suppression needs no cgo and the static
// CGO_ENABLED=0 build is preserved.
//
// The embedded rnnoise.wasm is compiled from Xiph's RNNoise, which is
// BSD-3-Clause; see COPYING in this directory, whose notice the licence
// requires us to carry with binary redistribution.
package denoise

import (
	"context"
	_ "embed"
	"encoding/binary"
	"fmt"
	"math"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

//go:embed rnnoise.wasm
var wasmModule []byte

const (
	// Rate and Hop are fixed by the kernel: 480 samples = 10 ms at 48 kHz.
	Rate = 48000
	Hop  = 480

	// DefaultStatesPerInstance is bound by CPU, not memory. A hop costs about
	// 0.19 ms against a 10 ms budget, and streams sharing an instance serialise
	// behind its lock, so 24 leaves roughly half the budget as headroom. The
	// module's own heap would hold far more, but exhausting it corrupts the
	// instance, and that ceiling is nowhere near this figure.
	DefaultStatesPerInstance = 24
)

// Kernel owns the compiled module and a pool of instances. Compilation is the
// expensive step and happens once; instantiation is cheap.
type Kernel struct {
	ctx         context.Context
	rt          wazero.Runtime
	cm          wazero.CompiledModule
	perInstance int

	mu        sync.Mutex
	instances []*instance
}

// NewKernel compiles the embedded module. It is the only step that can fail,
// and callers are expected to degrade to no denoising rather than abort.
func NewKernel(ctx context.Context, perInstance int) (*Kernel, error) {
	if perInstance <= 0 {
		perInstance = DefaultStatesPerInstance
	}
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigCompiler())
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		rt.Close(ctx)
		return nil, fmt.Errorf("wasi: %w", err)
	}
	cm, err := rt.CompileModule(ctx, wasmModule)
	if err != nil {
		rt.Close(ctx)
		return nil, fmt.Errorf("compile rnnoise.wasm: %w", err)
	}
	return &Kernel{ctx: ctx, rt: rt, cm: cm, perInstance: perInstance}, nil
}

func (k *Kernel) Close() error { return k.rt.Close(k.ctx) }

// Instances reports the number of pooled instances, for tests and metrics.
func (k *Kernel) Instances() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.instances)
}

// Stats reports live streams and pooled instances. Safe to call at any time,
// including from a metrics scrape.
func (k *Kernel) Stats() (streams, instances int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, in := range k.instances {
		streams += in.live
	}
	return streams, len(k.instances)
}

// instance is one wasm module instance. wazero cannot take concurrent calls
// into a module, so every call through it holds mu; the states sharing an
// instance therefore serialise, which is what bounds perInstance.
type instance struct {
	mu   sync.Mutex
	mod  api.Module
	mem  api.Memory
	live int

	malloc, free    api.Function
	create, destroy api.Function
	process         api.Function
}

func (k *Kernel) newInstance() (*instance, error) {
	cfg := wazero.NewModuleConfig().WithName("").WithStartFunctions("_initialize")
	mod, err := k.rt.InstantiateModule(k.ctx, k.cm, cfg)
	if err != nil {
		return nil, err
	}
	in := &instance{
		mod: mod, mem: mod.Memory(),
		malloc:  mod.ExportedFunction("malloc"),
		free:    mod.ExportedFunction("free"),
		create:  mod.ExportedFunction("rn_create"),
		destroy: mod.ExportedFunction("rn_destroy"),
		process: mod.ExportedFunction("rn_process"),
	}
	if in.malloc == nil || in.create == nil || in.process == nil {
		mod.Close(k.ctx)
		return nil, fmt.Errorf("rnnoise.wasm is missing expected exports")
	}
	return in, nil
}

func (in *instance) alloc(ctx context.Context, n uint32) (uint32, error) {
	r, err := in.malloc.Call(ctx, uint64(n))
	if err != nil {
		return 0, err
	}
	if r[0] == 0 {
		// The module's heap does not grow; a null here means it is exhausted,
		// and continuing past that point corrupts the whole instance.
		return 0, fmt.Errorf("rnnoise heap exhausted")
	}
	return uint32(r[0]), nil
}

// state is one stream's denoise state plus its scratch buffers, all living in
// the instance's linear memory.
type state struct {
	k       *Kernel
	in      *instance
	ptr     uint32
	inPtr   uint32
	outPtr  uint32
	scratch []byte
	// stack is reused across calls: api.Function.Call allocates a slice per
	// invocation, which on a 10 ms hop across hundreds of legs is GC pressure
	// for nothing. CallWithStack passes params in and returns results in place.
	stack [3]uint64
}

// acquire returns a state on an instance with spare capacity, creating an
// instance when every existing one is full.
func (k *Kernel) acquire() (*state, error) {
	k.mu.Lock()
	var in *instance
	for _, cand := range k.instances {
		if cand.live < k.perInstance {
			in = cand
			break
		}
	}
	if in == nil {
		fresh, err := k.newInstance()
		if err != nil {
			k.mu.Unlock()
			return nil, err
		}
		k.instances = append(k.instances, fresh)
		in = fresh
	}
	in.live++
	k.mu.Unlock()

	s, err := k.build(in)
	if err != nil {
		k.mu.Lock()
		in.live--
		k.mu.Unlock()
		return nil, err
	}
	return s, nil
}

func (k *Kernel) build(in *instance) (*state, error) {
	in.mu.Lock()
	defer in.mu.Unlock()

	r, err := in.create.Call(k.ctx)
	if err != nil {
		return nil, err
	}
	if r[0] == 0 {
		return nil, fmt.Errorf("rn_create returned null")
	}
	s := &state{k: k, in: in, ptr: uint32(r[0]), scratch: make([]byte, Hop*4)}
	if s.inPtr, err = in.alloc(k.ctx, Hop*4); err != nil {
		in.destroy.Call(k.ctx, uint64(s.ptr))
		return nil, err
	}
	if s.outPtr, err = in.alloc(k.ctx, Hop*4); err != nil {
		in.free.Call(k.ctx, uint64(s.inPtr))
		in.destroy.Call(k.ctx, uint64(s.ptr))
		return nil, err
	}
	return s, nil
}

// release returns a state's memory to its instance. Leaked states walk the
// instance toward a heap ceiling that nothing reclaims, so every acquired
// state must reach here.
func (s *state) release() {
	in := s.in
	in.mu.Lock()
	if s.ptr != 0 {
		in.destroy.Call(s.k.ctx, uint64(s.ptr))
		in.free.Call(s.k.ctx, uint64(s.inPtr))
		in.free.Call(s.k.ctx, uint64(s.outPtr))
		s.ptr = 0
	}
	in.mu.Unlock()

	s.k.mu.Lock()
	in.live--
	s.k.mu.Unlock()
}

// process denoises exactly Hop samples in place. RNNoise works in int16
// magnitude, which is what a PCM frame already carries, so no scaling is
// needed at the boundary.
func (s *state) process(frame []int16) error {
	if len(frame) != Hop {
		return fmt.Errorf("frame must be %d samples, got %d", Hop, len(frame))
	}
	in := s.in
	in.mu.Lock()
	defer in.mu.Unlock()
	if s.ptr == 0 {
		return fmt.Errorf("state released")
	}

	for i, v := range frame {
		binary.LittleEndian.PutUint32(s.scratch[i*4:], math.Float32bits(float32(v)))
	}
	if !in.mem.Write(s.inPtr, s.scratch) {
		return fmt.Errorf("input write out of range")
	}
	s.stack[0], s.stack[1], s.stack[2] = uint64(s.ptr), uint64(s.inPtr), uint64(s.outPtr)
	if err := in.process.CallWithStack(s.k.ctx, s.stack[:]); err != nil {
		return err
	}
	raw, ok := in.mem.Read(s.outPtr, Hop*4)
	if !ok {
		return fmt.Errorf("output read out of range")
	}
	for i := 0; i < Hop; i++ {
		v := float64(math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:])))
		if v > 32767 {
			v = 32767
		} else if v < -32768 {
			v = -32768
		}
		frame[i] = int16(v)
	}
	return nil
}
