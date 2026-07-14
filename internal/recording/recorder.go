package recording

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-audio/audio"
	"github.com/go-audio/wav"
	"github.com/google/uuid"
)

// Recorder captures PCM audio to a WAV file.
//
// When paused, the recorder keeps draining its input reader(s) but writes
// silence (zeroed samples) instead of the real audio. This preserves the
// recording's timeline so reviewers see a gap exactly where sensitive data
// was exchanged, rather than a shorter file that conceals it.
type Recorder struct {
	mu        sync.Mutex
	recording bool
	cancel    context.CancelFunc
	filePath  string
	done      chan struct{}
	log       *slog.Logger
	paused    atomic.Bool
}

func NewRecorder(log *slog.Logger) *Recorder {
	return &Recorder{log: log}
}

// Start begins recording from reader to a WAV file in dir at 8kHz sample rate.
// Returns the file path of the recording.
func (r *Recorder) Start(ctx context.Context, reader io.Reader, dir string) (string, error) {
	return r.StartAt(ctx, reader, dir, 8000)
}

// StartAt begins recording from reader to a mono WAV file in dir at the specified sample rate.
// Returns the file path of the recording.
func (r *Recorder) StartAt(ctx context.Context, reader io.Reader, dir string, sampleRate uint32) (string, error) {
	f, fpath, cancel, err := r.initRecording(ctx, dir)
	if err != nil {
		return "", err
	}

	go func() {
		// LIFO: clearRecording must run BEFORE close(r.done) so that callers
		// blocked on Wait() observe IsRecording()==false the moment they wake.
		defer close(r.done)
		defer r.clearRecording()
		err := r.recordMono(cancel.ctx, reader, f, int(sampleRate))
		if err != nil && cancel.ctx.Err() == nil {
			r.log.Error("recording error", "error", err)
		}
	}()

	return fpath, nil
}

// StartStereo begins a stereo recording with left and right channel readers.
// Left channel = participant's incoming audio, right channel = room mix.
//
// The right reader is the master clock and must be paced (written every frame,
// silence included); it alone determines the recording's timeline. The left
// reader is the companion: it may stall or burst, and must support TryRead so
// it can be drained without blocking. See recordStereo.
//
// Returns the file path of the recording.
func (r *Recorder) StartStereo(ctx context.Context, left, right io.Reader, dir string, sampleRate uint32) (string, error) {
	f, fpath, cancel, err := r.initRecording(ctx, dir)
	if err != nil {
		return "", err
	}

	go func() {
		// LIFO: clearRecording must run BEFORE close(r.done) so that callers
		// blocked on Wait() observe IsRecording()==false the moment they wake.
		defer close(r.done)
		defer r.clearRecording()
		err := r.recordStereo(cancel.ctx, left, right, f, int(sampleRate))
		if err != nil && cancel.ctx.Err() == nil {
			r.log.Error("stereo recording error", "error", err)
		}
	}()

	return fpath, nil
}

// cancelCtx bundles a context with its cancel function for passing to initRecording callers.
type cancelCtx struct {
	ctx    context.Context
	cancel context.CancelFunc
}

// initRecording sets up the recording file and state. Returns the open file,
// its path, and a cancellable context. The caller must call clearRecording when done.
func (r *Recorder) initRecording(ctx context.Context, dir string) (*os.File, string, cancelCtx, error) {
	r.mu.Lock()
	if r.recording {
		r.mu.Unlock()
		return nil, "", cancelCtx{}, fmt.Errorf("recording already in progress")
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		r.mu.Unlock()
		return nil, "", cancelCtx{}, fmt.Errorf("create recording dir: %w", err)
	}

	filename := fmt.Sprintf("%s_%s.wav", time.Now().Format("20060102_150405"), uuid.New().String()[:8])
	fpath := filepath.Join(dir, filename)

	f, err := os.Create(fpath)
	if err != nil {
		r.mu.Unlock()
		return nil, "", cancelCtx{}, fmt.Errorf("create recording file: %w", err)
	}

	ctx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	r.recording = true
	r.filePath = fpath
	r.done = make(chan struct{})
	r.mu.Unlock()

	return f, fpath, cancelCtx{ctx: ctx, cancel: cancel}, nil
}

// clearRecording resets the recorder state after recording finishes.
func (r *Recorder) clearRecording() {
	r.mu.Lock()
	r.recording = false
	r.cancel = nil
	r.mu.Unlock()
}

func (r *Recorder) Stop() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancel != nil {
		r.cancel()
	}
	return r.filePath
}

// Wait blocks until the recording goroutine has finished and the WAV file
// is fully written. Must be called after Stop.
func (r *Recorder) Wait() {
	r.mu.Lock()
	done := r.done
	r.mu.Unlock()
	if done != nil {
		<-done
	}
}

func (r *Recorder) IsRecording() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.recording
}

// Pause instructs the recorder to replace incoming audio with silence
// until Resume is called. Returns true if the state changed (i.e., the
// recorder was running and not already paused).
func (r *Recorder) Pause() bool {
	r.mu.Lock()
	running := r.recording
	r.mu.Unlock()
	if !running {
		return false
	}
	return r.paused.CompareAndSwap(false, true)
}

// Resume undoes a prior Pause. Returns true if the state changed.
func (r *Recorder) Resume() bool {
	r.mu.Lock()
	running := r.recording
	r.mu.Unlock()
	if !running {
		return false
	}
	return r.paused.CompareAndSwap(true, false)
}

// IsPaused reports whether the recorder is currently paused.
func (r *Recorder) IsPaused() bool {
	return r.paused.Load()
}

// recordMono writes raw PCM data as a mono WAV file using go-audio/wav.
// While paused, incoming samples are replaced with silence so the written
// WAV preserves real-time duration.
func (r *Recorder) recordMono(ctx context.Context, reader io.Reader, f *os.File, sampleRate int) error {
	defer f.Close()

	enc := wav.NewEncoder(f, sampleRate, 16, 1, 1) // mono, PCM format=1
	defer enc.Close()

	buf := make([]byte, 640)
	intBuf := &audio.IntBuffer{
		Format: &audio.Format{
			SampleRate:  sampleRate,
			NumChannels: 1,
		},
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		n, err := reader.Read(buf)
		if n > 0 {
			samples := bytesToInt(buf[:n])
			if r.paused.Load() {
				zeroInts(samples)
			}
			intBuf.Data = samples
			if werr := enc.Write(intBuf); werr != nil {
				return werr
			}
		}
		if err != nil {
			return nil
		}
	}
}

// tryReader is a reader that can be drained without blocking. The companion
// channel must satisfy it so that a companion with nothing to say cannot stall
// the master clock.
type tryReader interface {
	// TryRead reads whatever is immediately available, returning (0, nil) when
	// nothing is ready rather than waiting. It returns io.EOF only once the
	// source is closed and drained.
	TryRead(p []byte) (int, error)
}

// recordStereo writes a stereo WAV [L0, R0, L1, R1, ...] driven by the right
// reader's clock.
//
// The right channel is the master: it is fed continuously (silence included) by
// the leg's write loop or the room's mix tick, so each right frame read emits
// exactly one output slot and the file advances in real time. The left channel
// is the companion: it is written only when a packet actually arrives, so it is
// bursty and gap-prone. Reading it in lock-step with the master would park the
// loop the moment incoming audio stalled, while the master kept being written
// and silently dropped frames — leaving the two channels permanently skewed for
// the rest of the call. Instead the companion is drained without blocking into
// a bounded accumulator, and each slot either pops one companion frame or falls
// back to silence. The channels therefore stay sample-aligned across a stall.
//
// While paused, interleaved samples are zeroed on both channels.
func (r *Recorder) recordStereo(ctx context.Context, left, right io.Reader, f *os.File, sampleRate int) error {
	defer f.Close()

	// One slot is one 20 ms tap frame, the cadence both tap writers emit at.
	slotBytes := sampleRate / 50 * 2
	if slotBytes <= 0 {
		return fmt.Errorf("stereo recording: sample rate %d is too low", sampleRate)
	}
	slotSamples := slotBytes / 2

	// Both wired companions are pipe readers, which are non-blocking-capable.
	// Anything else would silence the left channel for the entire call, so say
	// so instead of quietly recording half a conversation.
	companion, ok := left.(tryReader)
	if !ok {
		return fmt.Errorf("stereo recording: companion reader %T cannot be read without blocking", left)
	}

	enc := wav.NewEncoder(f, sampleRate, 16, 2, 1) // stereo, PCM format=1
	defer enc.Close()

	masterBuf := make([]byte, slotBytes)
	drainBuf := make([]byte, slotBytes)
	silence := make([]byte, slotBytes)
	acc := newCompanionBuffer(slotBytes, companionMaxSlots)

	intBuf := &audio.IntBuffer{
		Format: &audio.Format{
			SampleRate:  sampleRate,
			NumChannels: 2,
		},
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		// The master paces the recording: one full frame in, one slot out.
		if _, rerr := io.ReadFull(right, masterBuf); rerr != nil {
			return nil
		}

		// Take whatever the companion has ready right now, and no more.
		for {
			n, cerr := companion.TryRead(drainBuf)
			if n > 0 {
				acc.append(drainBuf[:n])
			}
			if cerr != nil || n == 0 {
				break
			}
		}

		leftSlot, popped := acc.pop()
		if !popped {
			leftSlot = silence
		}

		interleaved := make([]int, slotSamples*2)
		if !r.paused.Load() {
			for i := 0; i < slotSamples; i++ {
				interleaved[i*2] = int(int16(binary.LittleEndian.Uint16(leftSlot[i*2:])))
				interleaved[i*2+1] = int(int16(binary.LittleEndian.Uint16(masterBuf[i*2:])))
			}
		}
		intBuf.Data = interleaved
		if werr := enc.Write(intBuf); werr != nil {
			return werr
		}
	}
}

// companionMaxSlots bounds the companion accumulator at 16 slots (~320 ms of
// audio). The companion only builds a backlog when it briefly outruns the
// master clock; anything past this is stale enough that keeping it would just
// push the whole channel late for the rest of the call.
const companionMaxSlots = 16

// companionBuffer accumulates companion-channel bytes between master frames.
// It is bounded: once it holds more than maxSlots whole slots the oldest slot
// is dropped, so a companion that outruns the master clock loses its stalest
// audio instead of growing without limit.
//
// It is not safe for concurrent use; the recording loop owns it.
type companionBuffer struct {
	buf       []byte
	slotBytes int
	maxSlots  int
}

func newCompanionBuffer(slotBytes, maxSlots int) *companionBuffer {
	return &companionBuffer{slotBytes: slotBytes, maxSlots: maxSlots}
}

// append adds b, dropping whole slots from the front while over the bound.
func (c *companionBuffer) append(b []byte) {
	c.buf = append(c.buf, b...)
	bound := c.maxSlots * c.slotBytes
	for len(c.buf) > bound {
		c.buf = c.buf[c.slotBytes:]
	}
}

// pop removes and returns the oldest whole slot. It reports false when a full
// slot is not available, which is the caller's cue to emit silence instead.
//
// The returned slot aliases the buffer and stays valid until the next append;
// callers consume it before accumulating more.
func (c *companionBuffer) pop() ([]byte, bool) {
	if len(c.buf) < c.slotBytes {
		return nil, false
	}
	slot := c.buf[:c.slotBytes]
	c.buf = c.buf[c.slotBytes:]
	return slot, true
}

// size reports how many bytes are currently held.
func (c *companionBuffer) size() int { return len(c.buf) }

// zeroInts sets every element of s to 0.
func zeroInts(s []int) {
	for i := range s {
		s[i] = 0
	}
}

// bytesToInt converts little-endian 16-bit PCM bytes to []int.
func bytesToInt(b []byte) []int {
	n := len(b) / 2
	out := make([]int, n)
	for i := 0; i < n; i++ {
		out[i] = int(int16(binary.LittleEndian.Uint16(b[i*2:])))
	}
	return out
}
