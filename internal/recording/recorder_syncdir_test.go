package recording

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-audio/audio"
	"github.com/go-audio/wav"
)

// writeCompleteWAV writes one frame to f through a wav.Encoder and closes the
// encoder, which rewrites the RIFF/data sizes. The result is a complete,
// playable WAV on disk; f itself is left open, the way finish receives it.
func writeCompleteWAV(t *testing.T, f *os.File) {
	t.Helper()
	enc := wav.NewEncoder(f, 8000, 16, 1, 1)
	if err := enc.Write(&audio.IntBuffer{
		Format: &audio.Format{SampleRate: 8000, NumChannels: 1},
		Data:   make([]int, 320),
	}); err != nil {
		t.Fatalf("encode frame: %v", err)
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("close encoder: %v", err)
	}
}

// TestRecorder_FinalizedRecordingSurvivesPublishError pins the durability-not-
// guaranteed branch of finish. publishFile's rename is the point of no return:
// once a complete recording has landed at its final path, a failure in the
// closing directory sync leaves the file present and readable, just not known to
// survive a crash. That is not a corrupt capture, and gating on Published alone
// discarded it — losing a complete recording and skipping its upload.
//
// A real directory fsync failure is not inducible under the test's privileges,
// so the post-rename state is reconstructed the way the suite builds staged
// files elsewhere: the file is renamed into place first, then finish runs, so
// publishFile errors after the recording is already at its final path — exactly
// the state a failed syncDir leaves behind. What finish must key on is the file
// being present there, not the specific error.
func TestRecorder_FinalizedRecordingSurvivesPublishError(t *testing.T) {
	dir := t.TempDir()
	fpath := filepath.Join(dir, "degraded.wav")

	staged, err := createStagedFile(fpath)
	if err != nil {
		t.Fatalf("createStagedFile: %v", err)
	}
	writeCompleteWAV(t, staged.f)

	// The recording has already landed at its final path, as it has once
	// publishFile's rename returns. publishFile will then error (its own rename
	// finds no staging file), standing in for the closing directory sync failing
	// after the rename succeeded.
	if err := os.Rename(staged.tmpPath, staged.finalPath); err != nil {
		t.Fatalf("stage rename: %v", err)
	}

	r := NewRecorder(slog.Default())
	r.finish(staged, true, nil)

	if !r.Finalized() {
		t.Error("Finalized() is false though a complete recording is present at its final path")
	}
	if r.Published() {
		t.Error("Published() is true though the publish returned an error")
	}
	if _, err := os.Stat(fpath); err != nil {
		t.Errorf("%s is missing after a publish that only failed its directory sync: %v", fpath, err)
	}
	assertPlayable(t, fpath, 1)
	assertNoStagingResidue(t, dir)
}

// TestRecorder_PublishErrorWithoutFileIsNotFinalized guards the original
// contract: a publish that fails before anything lands at the final path leaves
// no usable file, so it must not be reported as finalized. Only the presence of
// the file at the final path distinguishes it from the degraded-durability case
// above.
func TestRecorder_PublishErrorWithoutFileIsNotFinalized(t *testing.T) {
	dir := t.TempDir()
	fpath := filepath.Join(dir, "gone.wav")

	staged, err := createStagedFile(fpath)
	if err != nil {
		t.Fatalf("createStagedFile: %v", err)
	}
	writeCompleteWAV(t, staged.f)

	// Drop the staging file so publishFile's rename fails with nothing at the
	// final path — the shape of a failure at or before the rename. The capture
	// itself reported no error, so the failure comes from publishFile alone.
	if err := os.Remove(staged.tmpPath); err != nil {
		t.Fatalf("remove staging file: %v", err)
	}

	r := NewRecorder(slog.Default())
	r.finish(staged, true, nil)

	if r.Finalized() {
		t.Error("Finalized() is true though nothing landed at the final path")
	}
	if r.Published() {
		t.Error("Published() is true though the publish failed")
	}
	if _, err := os.Stat(fpath); !os.IsNotExist(err) {
		t.Errorf("final path exists after a publish that never renamed, os.Stat err = %v", err)
	}
	assertNoStagingResidue(t, dir)
}
