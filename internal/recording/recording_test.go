package recording

import (
	"bytes"
	"context"
	"encoding/binary"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
