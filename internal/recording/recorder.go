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
		defer r.clearRecording()
		defer close(r.done)
		err := r.recordMono(cancel.ctx, reader, f, int(sampleRate))
		if err != nil && cancel.ctx.Err() == nil {
			r.log.Error("recording error", "error", err)
		}
	}()

	return fpath, nil
}

// StartStereo begins a stereo recording with left and right channel readers.
// Left channel = participant's incoming audio, right channel = room mix.
// Returns the file path of the recording.
func (r *Recorder) StartStereo(ctx context.Context, left, right io.Reader, dir string, sampleRate uint32) (string, error) {
	f, fpath, cancel, err := r.initRecording(ctx, dir)
	if err != nil {
		return "", err
	}

	go func() {
		defer r.clearRecording()
		defer close(r.done)
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

// recordStereo reads one frame at a time from left and right readers,
// interleaves the samples [L0, R0, L1, R1, ...], and writes a stereo WAV file.
// While paused, interleaved samples are zeroed.
func (r *Recorder) recordStereo(ctx context.Context, left, right io.Reader, f *os.File, sampleRate int) error {
	defer f.Close()

	enc := wav.NewEncoder(f, sampleRate, 16, 2, 1) // stereo, PCM format=1
	defer enc.Close()

	const frameSizeBytes = 640 // 320 samples * 2 bytes per sample
	leftBuf := make([]byte, frameSizeBytes)
	rightBuf := make([]byte, frameSizeBytes)

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

		ln, lerr := io.ReadFull(left, leftBuf)
		rn, rerr := io.ReadFull(right, rightBuf)

		// Use whichever has fewer samples to stay aligned.
		nSamples := ln / 2
		if rn/2 < nSamples {
			nSamples = rn / 2
		}

		if nSamples > 0 {
			interleaved := make([]int, nSamples*2)
			if !r.paused.Load() {
				for i := 0; i < nSamples; i++ {
					interleaved[i*2] = int(int16(binary.LittleEndian.Uint16(leftBuf[i*2:])))
					interleaved[i*2+1] = int(int16(binary.LittleEndian.Uint16(rightBuf[i*2:])))
				}
			}
			intBuf.Data = interleaved
			if werr := enc.Write(intBuf); werr != nil {
				return werr
			}
		}

		if lerr != nil || rerr != nil {
			return nil
		}
	}
}

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
