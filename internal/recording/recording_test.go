package recording

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNewRecorder(t *testing.T) {
	r := NewRecorder(slog.Default())
	if r == nil {
		t.Fatal("expected non-nil recorder")
	}
	if r.IsRecording() {
		t.Error("new recorder should not be recording")
	}
}

func TestRecorder_StartStop(t *testing.T) {
	dir := t.TempDir()
	r := NewRecorder(slog.Default())

	// Provide PCM data to read.
	pcm := generatePCM(8000, 1) // 1 second of silence
	reader := bytes.NewReader(pcm)

	fpath, err := r.Start(context.Background(), reader, dir)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if fpath == "" {
		t.Fatal("expected non-empty file path")
	}
	if !r.IsRecording() {
		t.Error("expected IsRecording=true")
	}

	path := r.Stop()
	r.Wait()

	if path == "" {
		t.Error("Stop returned empty path")
	}
	if r.IsRecording() {
		t.Error("expected IsRecording=false after stop")
	}

	// Verify file exists.
	info, err := os.Stat(fpath)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size() == 0 {
		t.Error("expected non-empty WAV file")
	}
}

func TestRecorder_DoubleStart(t *testing.T) {
	dir := t.TempDir()
	r := NewRecorder(slog.Default())

	reader := bytes.NewReader(generatePCM(8000, 1))
	_, err := r.Start(context.Background(), reader, dir)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	_, err = r.Start(context.Background(), bytes.NewReader(nil), dir)
	if err == nil {
		t.Error("expected error on double start")
	}

	r.Stop()
	r.Wait()
}

func TestRecorder_StopBeforeStart(t *testing.T) {
	r := NewRecorder(slog.Default())
	path := r.Stop() // should not panic
	if path != "" {
		t.Errorf("expected empty path, got %q", path)
	}
}

func TestRecorder_WAVHeader(t *testing.T) {
	dir := t.TempDir()
	r := NewRecorder(slog.Default())

	pcm := generatePCM(8000, 1)
	reader := bytes.NewReader(pcm)

	fpath, err := r.Start(context.Background(), reader, dir)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Let it read some data then stop.
	time.Sleep(100 * time.Millisecond)
	r.Stop()
	r.Wait()

	// Read the file and check the RIFF header.
	data, err := os.ReadFile(fpath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(data) < 44 {
		t.Fatalf("file too small: %d bytes", len(data))
	}
	if string(data[:4]) != "RIFF" {
		t.Errorf("expected RIFF header, got %q", data[:4])
	}
	if string(data[8:12]) != "WAVE" {
		t.Errorf("expected WAVE format, got %q", data[8:12])
	}
}

func TestRecorder_StartAt_CustomRate(t *testing.T) {
	dir := t.TempDir()
	r := NewRecorder(slog.Default())

	pcm := generatePCM(16000, 1)
	reader := bytes.NewReader(pcm)

	fpath, err := r.StartAt(context.Background(), reader, dir, 16000)
	if err != nil {
		t.Fatalf("StartAt: %v", err)
	}

	time.Sleep(100 * time.Millisecond)
	r.Stop()
	r.Wait()

	info, err := os.Stat(fpath)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size() == 0 {
		t.Error("expected non-empty file")
	}
}

func TestRecorder_ContextCancel(t *testing.T) {
	dir := t.TempDir()
	r := NewRecorder(slog.Default())

	ctx, cancel := context.WithCancel(context.Background())

	// Use a reader that blocks forever.
	reader := &blockingReader{}

	_, err := r.Start(ctx, reader, dir)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	cancel()
	r.Wait()

	if r.IsRecording() {
		t.Error("expected not recording after context cancel")
	}
}

func TestRecorder_CreatesDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sub", "dir")
	r := NewRecorder(slog.Default())

	reader := bytes.NewReader(generatePCM(8000, 1))
	_, err := r.Start(context.Background(), reader, dir)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	r.Stop()
	r.Wait()

	// Verify subdir was created.
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("directory not created: %v", err)
	}
}

func TestRecorder_FileNameFormat(t *testing.T) {
	dir := t.TempDir()
	r := NewRecorder(slog.Default())

	reader := bytes.NewReader(generatePCM(8000, 1))
	fpath, err := r.Start(context.Background(), reader, dir)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	r.Stop()
	r.Wait()

	base := filepath.Base(fpath)
	if !strings.HasSuffix(base, ".wav") {
		t.Errorf("expected .wav suffix: %q", base)
	}
	if !strings.Contains(base, "_") {
		t.Errorf("expected underscore in filename: %q", base)
	}
}

// --- Pause / Resume ---

func TestRecorder_PauseResume_StateTransitions(t *testing.T) {
	r := NewRecorder(slog.Default())

	// Can't pause when not recording.
	if r.Pause() {
		t.Error("Pause should return false when not recording")
	}
	if r.Resume() {
		t.Error("Resume should return false when not recording")
	}

	dir := t.TempDir()
	_, err := r.Start(context.Background(), &blockingReader{}, dir)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		r.Stop()
		r.Wait()
	}()

	if r.IsPaused() {
		t.Error("new recording should not be paused")
	}
	if !r.Pause() {
		t.Error("first Pause should return true")
	}
	if !r.IsPaused() {
		t.Error("expected IsPaused after Pause")
	}
	if r.Pause() {
		t.Error("second Pause should return false (already paused)")
	}
	if !r.Resume() {
		t.Error("first Resume should return true")
	}
	if r.IsPaused() {
		t.Error("expected !IsPaused after Resume")
	}
	if r.Resume() {
		t.Error("second Resume should return false (already resumed)")
	}
}

// TestRecorder_Pause_WritesSilence verifies that audio written while the
// recorder is paused appears as silence in the output WAV, while the file's
// total duration still covers the whole session (timeline preserved).
func TestRecorder_Pause_WritesSilence(t *testing.T) {
	dir := t.TempDir()
	r := NewRecorder(slog.Default())

	// Feed 0.5s of non-zero audio, pause, feed 0.5s of non-zero audio,
	// resume, feed 0.5s of non-zero audio. Expect ~1.5s duration where
	// the middle segment is silent in the output.
	sampleRate := 8000
	seg := make([]byte, sampleRate) // 0.5s of int16 samples (2 bytes each -> 0.25s... wait)
	// 0.5 s at 8 kHz mono 16-bit = 8000 samples = 16000 bytes.
	seg = make([]byte, sampleRate*2/2) // 0.5 s
	for i := 0; i < len(seg); i += 2 {
		// Write a constant non-zero sample (amplitude 0x0FFF).
		seg[i] = 0xff
		seg[i+1] = 0x0f
	}

	pr, pw := newSyncPipe()

	fpath, err := r.Start(context.Background(), pr, dir)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	pw.Write(seg) // audible
	// Give the recorder goroutine a moment to drain.
	time.Sleep(50 * time.Millisecond)

	r.Pause()
	pw.Write(seg) // should be silenced
	time.Sleep(50 * time.Millisecond)

	r.Resume()
	pw.Write(seg) // audible again
	time.Sleep(50 * time.Millisecond)

	pw.Close()
	r.Stop()
	r.Wait()

	data, err := os.ReadFile(fpath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	// Parse samples from the PCM data block (skip the 44-byte WAV header).
	if len(data) < 44 {
		t.Fatalf("WAV too small: %d bytes", len(data))
	}
	pcm := data[44:]
	totalSamples := len(pcm) / 2
	if totalSamples < sampleRate*3/2*9/10 {
		// Expect ~12000 samples (1.5s @ 8kHz); allow 10% slack for scheduling.
		t.Errorf("too few samples: %d (want ~%d)", totalSamples, sampleRate*3/2)
	}

	// Count zero vs non-zero samples: we expect a meaningful fraction to be
	// zero (the paused segment) and a meaningful fraction to be non-zero.
	zero, nonzero := 0, 0
	for i := 0; i+1 < len(pcm); i += 2 {
		s := int16(binary.LittleEndian.Uint16(pcm[i:]))
		if s == 0 {
			zero++
		} else {
			nonzero++
		}
	}
	if zero == 0 {
		t.Error("expected some silent samples from paused segment, got 0")
	}
	if nonzero == 0 {
		t.Error("expected some non-zero samples from unpaused segments, got 0")
	}
}

// --- bytesToInt tests ---

func TestBytesToInt(t *testing.T) {
	// Encode some known int16 samples.
	samples := []int16{0, 1000, -1000, 32767, -32768}
	buf := make([]byte, len(samples)*2)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(s))
	}

	got := bytesToInt(buf)
	if len(got) != len(samples) {
		t.Fatalf("len = %d, want %d", len(got), len(samples))
	}
	for i, want := range samples {
		if got[i] != int(want) {
			t.Errorf("got[%d] = %d, want %d", i, got[i], want)
		}
	}
}

func TestBytesToInt_Empty(t *testing.T) {
	got := bytesToInt(nil)
	if len(got) != 0 {
		t.Errorf("expected empty, got %d", len(got))
	}
}

// --- Helpers ---

// generatePCM creates silent 16-bit LE PCM data for the given sample rate and duration.
func generatePCM(sampleRate, seconds int) []byte {
	numSamples := sampleRate * seconds
	buf := make([]byte, numSamples*2)
	return buf
}

// blockingReader blocks forever on Read until the context is cancelled.
type blockingReader struct{}

func (r *blockingReader) Read(p []byte) (int, error) {
	// Block forever (the recorder should be cancelled via context).
	select {}
}

// syncPipe is a simple in-memory pipe used by tests to feed bytes into the
// recorder. Writes enqueue to a buffered channel; Read dequeues.
type syncPipeReader struct {
	ch   chan []byte
	done chan struct{}
	buf  []byte
}

type syncPipeWriter struct {
	ch     chan []byte
	done   chan struct{}
	closed bool
	mu     sync.Mutex
}

func newSyncPipe() (*syncPipeReader, *syncPipeWriter) {
	ch := make(chan []byte, 64)
	done := make(chan struct{})
	return &syncPipeReader{ch: ch, done: done}, &syncPipeWriter{ch: ch, done: done}
}

func (r *syncPipeReader) Read(p []byte) (int, error) {
	if len(r.buf) > 0 {
		n := copy(p, r.buf)
		r.buf = r.buf[n:]
		return n, nil
	}
	select {
	case data, ok := <-r.ch:
		if !ok {
			return 0, io.EOF
		}
		n := copy(p, data)
		if n < len(data) {
			r.buf = data[n:]
		}
		return n, nil
	case <-r.done:
		return 0, io.EOF
	}
}

func (w *syncPipeWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, io.ErrClosedPipe
	}
	cp := make([]byte, len(p))
	copy(cp, p)
	w.ch <- cp
	return len(p), nil
}

func (w *syncPipeWriter) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	w.closed = true
	close(w.done)
}
