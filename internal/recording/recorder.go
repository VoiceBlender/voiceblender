package recording

import (
	"context"
	"encoding/binary"
	"errors"
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
	published bool
	finalized bool
	done      chan struct{}
	log       *slog.Logger
	paused    atomic.Bool
	// newTicker supplies the stereo recording clock. Tests set it to drive the
	// loop deterministically instead of in real time.
	newTicker func() (<-chan time.Time, func())
}

func NewRecorder(log *slog.Logger) *Recorder {
	return &Recorder{log: log}
}

// Start begins recording from reader to a WAV file in dir at 8kHz sample rate.
// Returns the file path of the recording.
func (r *Recorder) Start(ctx context.Context, reader io.Reader, dir string) (string, error) {
	return r.StartAt(ctx, reader, dir, 8000, "")
}

// StartAt begins recording from reader to a mono WAV file in dir at the specified sample rate.
// When basename is non-empty it must already be sanitized (see SanitizeBasename).
// Returns the file path of the recording.
func (r *Recorder) StartAt(ctx context.Context, reader io.Reader, dir string, sampleRate uint32, basename string) (string, error) {
	staged, cancel, err := r.initRecording(ctx, dir, basename)
	if err != nil {
		return "", err
	}

	go func() {
		// LIFO: clearRecording must run BEFORE close(r.done) so that callers
		// blocked on Wait() observe IsRecording()==false the moment they wake.
		defer close(r.done)
		defer r.clearRecording()
		wrote, err := r.recordMono(cancel.ctx, reader, staged.f, int(sampleRate))
		if err != nil && cancel.ctx.Err() == nil {
			r.log.Error("recording error", "error", err)
		}
		// Publishing runs ahead of the deferred handshake so that Wait callers
		// find the recording at its final path the moment they wake.
		r.finish(staged, wrote, err)
	}()

	return staged.finalPath, nil
}

// StartStereo begins a stereo recording with left and right channel readers.
// Left channel = participant's incoming audio, right channel = room mix.
//
// Both readers must support TryRead so neither can stall the other; the
// recording is paced by its own clock. See recordStereo.
//
// Returns the file path of the recording.
func (r *Recorder) StartStereo(ctx context.Context, left, right io.Reader, dir string, sampleRate uint32, basename string) (string, error) {
	staged, cancel, err := r.initRecording(ctx, dir, basename)
	if err != nil {
		return "", err
	}

	go func() {
		// LIFO: clearRecording must run BEFORE close(r.done) so that callers
		// blocked on Wait() observe IsRecording()==false the moment they wake.
		defer close(r.done)
		defer r.clearRecording()
		wrote, err := r.recordStereo(cancel.ctx, left, right, staged.f, int(sampleRate))
		if err != nil && cancel.ctx.Err() == nil {
			r.log.Error("stereo recording error", "error", err)
		}
		// Publishing runs ahead of the deferred handshake so that Wait callers
		// find the recording at its final path the moment they wake.
		r.finish(staged, wrote, err)
	}()

	return staged.finalPath, nil
}

// cancelCtx bundles a context with its cancel function for passing to initRecording callers.
type cancelCtx struct {
	ctx    context.Context
	cancel context.CancelFunc
}

// initRecording sets up the recording file and state. The path it hands back
// names a file that does not exist yet: the capture goes to a staging file and
// only appears there once the caller publishes it. The caller must call
// clearRecording when done.
func (r *Recorder) initRecording(ctx context.Context, dir string, basename string) (*stagedFile, cancelCtx, error) {
	r.mu.Lock()
	if r.recording {
		r.mu.Unlock()
		return nil, cancelCtx{}, fmt.Errorf("recording already in progress")
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		r.mu.Unlock()
		return nil, cancelCtx{}, fmt.Errorf("create recording dir: %w", err)
	}

	filename, err := resolveRecordingBasename(basename)
	if err != nil {
		r.mu.Unlock()
		return nil, cancelCtx{}, fmt.Errorf("recording filename: %w", err)
	}
	fpath := filepath.Join(dir, filename)
	if err := reserveFinalPath(fpath); err != nil {
		r.mu.Unlock()
		return nil, cancelCtx{}, err
	}

	staged, err := createStagedFile(fpath)
	if err != nil {
		releaseFinalPath(fpath)
		r.mu.Unlock()
		return nil, cancelCtx{}, fmt.Errorf("create recording file: %w", err)
	}

	ctx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	r.recording = true
	r.filePath = fpath
	r.published = false
	r.finalized = false
	r.done = make(chan struct{})
	r.mu.Unlock()

	return staged, cancelCtx{ctx: ctx, cancel: cancel}, nil
}

// finish publishes or discards the staged recording once the capture loop has
// returned. A capture that never wrote a frame is discarded even though it
// reports no error: the encoder only emits the RIFF header on its first write,
// so its file is not a readable WAV.
func (r *Recorder) finish(staged *stagedFile, wrote bool, recErr error) {
	// Always drop the in-process reservation. A published file still blocks
	// reuse via os.Stat; a discarded capture frees the name for a later start.
	defer releaseFinalPath(staged.finalPath)

	if recErr != nil || !wrote {
		discardTemp(staged.f, staged.tmpPath)
		return
	}
	err := publishFile(staged.f, staged.tmpPath, staged.finalPath)
	// The link is publishFile's point of no return: a failure after it leaves a
	// complete recording at the final path, so a file present there means the
	// recording is finalized even though publishFile errored.
	finalized := err == nil
	if err != nil {
		if _, statErr := os.Stat(staged.finalPath); statErr == nil {
			finalized = true
			r.log.Warn("recording published with degraded durability", "error", err, "file", staged.finalPath)
		} else {
			r.log.Error("publish recording", "error", err, "file", staged.finalPath)
		}
	}
	r.mu.Lock()
	r.finalized = finalized
	r.published = err == nil
	r.mu.Unlock()
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

// Published reports whether the recording reached the path Stop returns AND is
// known to survive a crash. It is false while a recording is running, and false
// afterwards for a discarded capture or a publish that failed only at the
// closing directory sync. Callers deciding whether a usable file exists want
// Finalized instead.
func (r *Recorder) Published() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.published
}

// Finalized reports whether a complete, readable recording exists at the path
// Stop returns. Unlike Published it stays true when only the closing directory
// sync failed: the file is there and readable, merely not known to survive a
// crash. This is the gate for reporting a recording or handing it on.
func (r *Recorder) Finalized() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.finalized
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
//
// The file is left open on return: the caller publishes or discards it. Closing
// the encoder is what rewrites the WAV size header, so a close failure is
// reported as a capture error and the file is discarded rather than published
// unreadable. wrote reports whether any frame reached the encoder; without one
// the file has no header at all and is discarded for the same reason.
func (r *Recorder) recordMono(ctx context.Context, reader io.Reader, f *os.File, sampleRate int) (wrote bool, err error) {
	enc := wav.NewEncoder(f, sampleRate, 16, 1, 1) // mono, PCM format=1
	// A capture error wins over a close error: it is the earlier and more
	// specific failure, and both lead to the same discard.
	defer func() {
		if cerr := enc.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close recording: %w", cerr)
		}
	}()

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
			return wrote, nil
		default:
		}
		n, rerr := reader.Read(buf)
		if n > 0 {
			samples := bytesToInt(buf[:n])
			if r.paused.Load() {
				zeroInts(samples)
			}
			intBuf.Data = samples
			if werr := enc.Write(intBuf); werr != nil {
				return wrote, werr
			}
			wrote = true
		}
		if rerr != nil {
			return wrote, nil
		}
	}
}

// tryReader is a reader that can be drained without blocking. Both stereo
// channels must satisfy it so that a silent producer cannot stall the loop.
type tryReader interface {
	// TryRead reads whatever is immediately available, returning (0, nil) when
	// nothing is ready rather than waiting. It returns io.EOF only once the
	// source is closed and drained.
	TryRead(p []byte) (int, error)
}

// slotInterval is one output slot, matching the 20 ms cadence the tap writers
// emit at.
const slotInterval = 20 * time.Millisecond

// maxSlotBacklog bounds each channel's accumulator at 16 slots (~320 ms). A
// channel only builds a backlog when it briefly outruns the recording clock;
// audio older than that is stale enough that keeping it would push the channel
// late for the rest of the call.
const maxSlotBacklog = 16

// slotTicker returns the recording clock and a stop function.
func (r *Recorder) slotTicker() (<-chan time.Time, func()) {
	if r.newTicker != nil {
		return r.newTicker()
	}
	t := time.NewTicker(slotInterval)
	return t.C, t.Stop
}

// recordStereo writes a stereo WAV [L0, R0, L1, R1, ...] paced by its own
// 20 ms clock.
//
// Neither channel paces the other. Both are drained without blocking into
// bounded accumulators, and every tick emits one slot that pops a frame from
// each, or silence where a channel had none ready. Borrowing a producer's
// cadence instead would freeze the whole recording whenever that producer went
// quiet — which the wired ones do: a held SIP leg and an outbound DTMF burst
// both skip the leg's out-tap write, and the mixer skips the out-tap of a
// deafened or write-only participant. Anchoring both channels to the same wall
// clock keeps them sample-aligned and keeps the file in real time regardless.
//
// While paused, interleaved samples are zeroed on both channels. The file is
// left open on return and the caller publishes or discards it, on the same terms
// as recordMono.
func (r *Recorder) recordStereo(ctx context.Context, left, right io.Reader, f *os.File, sampleRate int) (wrote bool, err error) {
	// Derived from slotInterval so the slot and the tick that consumes it cannot
	// drift apart.
	slotBytes := sampleRate * int(slotInterval/time.Millisecond) / 1000 * 2
	if slotBytes <= 0 {
		return false, fmt.Errorf("stereo recording: sample rate %d is too low", sampleRate)
	}
	slotSamples := slotBytes / 2

	leftIn, ok := left.(tryReader)
	if !ok {
		return false, fmt.Errorf("stereo recording: left reader %T cannot be read without blocking", left)
	}
	rightIn, ok := right.(tryReader)
	if !ok {
		return false, fmt.Errorf("stereo recording: right reader %T cannot be read without blocking", right)
	}

	enc := wav.NewEncoder(f, sampleRate, 16, 2, 1) // stereo, PCM format=1
	// A capture error wins over a close error: it is the earlier and more
	// specific failure, and both lead to the same discard.
	defer func() {
		if cerr := enc.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close recording: %w", cerr)
		}
	}()

	tick, stopTick := r.slotTicker()
	defer stopTick()

	drainBuf := make([]byte, slotBytes)
	silence := make([]byte, slotBytes)
	leftAcc := newSlotBuffer(slotBytes, maxSlotBacklog)
	rightAcc := newSlotBuffer(slotBytes, maxSlotBacklog)

	intBuf := &audio.IntBuffer{
		Format: &audio.Format{
			SampleRate:  sampleRate,
			NumChannels: 2,
		},
	}

	// drain reports whether both channels are closed and drained, which is the
	// only end condition besides cancellation: with nothing left to arrive there
	// is nothing more to record.
	drain := func() bool {
		leftEOF := drainInto(leftIn, leftAcc, drainBuf)
		rightEOF := drainInto(rightIn, rightAcc, drainBuf)
		return leftEOF && rightEOF
	}

	emit := func() error {
		leftSlot, ok := leftAcc.pop()
		if !ok {
			leftSlot = silence
		}
		rightSlot, ok := rightAcc.pop()
		if !ok {
			rightSlot = silence
		}
		interleaved := make([]int, slotSamples*2)
		if !r.paused.Load() {
			for i := 0; i < slotSamples; i++ {
				interleaved[i*2] = int(int16(binary.LittleEndian.Uint16(leftSlot[i*2:])))
				interleaved[i*2+1] = int(int16(binary.LittleEndian.Uint16(rightSlot[i*2:])))
			}
		}
		intBuf.Data = interleaved
		if werr := enc.Write(intBuf); werr != nil {
			return werr
		}
		wrote = true
		return nil
	}

	// Ticks before either producer has spoken are not recorded: leading silence
	// would offset the whole timeline, and a capture that never sees a frame must
	// stay empty so the caller discards it instead of publishing silence.
	started := false

	// flush writes out whatever whole slots are still buffered, so teardown does
	// not lose the tail.
	flush := func() error {
		for started && (leftAcc.size() >= slotBytes || rightAcc.size() >= slotBytes) {
			if werr := emit(); werr != nil {
				return werr
			}
		}
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			drain()
			return wrote, flush()
		case <-tick:
		}

		closed := drain()
		if !started {
			if leftAcc.size() < slotBytes && rightAcc.size() < slotBytes {
				if closed {
					return wrote, nil
				}
				continue
			}
			started = true
		}
		if werr := emit(); werr != nil {
			return wrote, werr
		}
		if closed {
			return wrote, flush()
		}
	}
}

// drainInto moves everything src has ready right now into acc, and no more. It
// reports whether src is closed and drained.
func drainInto(src tryReader, acc *slotBuffer, buf []byte) bool {
	for {
		n, err := src.TryRead(buf)
		if n > 0 {
			acc.append(buf[:n])
		}
		if err != nil {
			return errors.Is(err, io.EOF)
		}
		if n == 0 {
			return false
		}
	}
}

// slotBuffer accumulates one channel's bytes between ticks. It is bounded: once
// it holds more than maxSlots whole slots the oldest slot is dropped, so a
// channel that outruns the recording clock loses its stalest audio instead of
// growing without limit.
//
// It is not safe for concurrent use; the recording loop owns it.
type slotBuffer struct {
	buf       []byte
	slotBytes int
	maxSlots  int
}

func newSlotBuffer(slotBytes, maxSlots int) *slotBuffer {
	return &slotBuffer{slotBytes: slotBytes, maxSlots: maxSlots}
}

// append adds b, dropping whole slots from the front while over the bound.
func (c *slotBuffer) append(b []byte) {
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
func (c *slotBuffer) pop() ([]byte, bool) {
	if len(c.buf) < c.slotBytes {
		return nil, false
	}
	slot := c.buf[:c.slotBytes]
	c.buf = c.buf[c.slotBytes:]
	return slot, true
}

// size reports how many bytes are currently held.
func (c *slotBuffer) size() int { return len(c.buf) }

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
