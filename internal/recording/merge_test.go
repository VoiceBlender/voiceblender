package recording

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/go-audio/audio"
	"github.com/go-audio/wav"
)

// writeMonoWAV lays down a mono WAV for the merge inputs.
func writeMonoWAV(t *testing.T, path string, sampleRate, samples int) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()

	enc := wav.NewEncoder(f, sampleRate, 16, 1, 1)
	buf := &audio.IntBuffer{
		Format:         &audio.Format{SampleRate: sampleRate, NumChannels: 1},
		Data:           make([]int, samples),
		SourceBitDepth: 16,
	}
	for i := range buf.Data {
		buf.Data[i] = 100 + i%50
	}
	if err := enc.Write(buf); err != nil {
		t.Fatalf("encode %s: %v", path, err)
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("close encoder for %s: %v", path, err)
	}
}

// TestMergeMultiChannel_PublishesDurably covers the merge output through the
// same durable publish as a captured recording.
func TestMergeMultiChannel_PublishesDurably(t *testing.T) {
	const sampleRate = 16000

	// The restrictive umask is what gives assertPublishedMode teeth. Both halves
	// of the staged publish are umask-independent — os.CreateTemp always opens
	// 0600, and the explicit chmod always restores 0644 — whereas a merge that
	// created its output directly would get 0666 &^ umask, i.e. 0600 here. So
	// under this umask only a genuinely staged-and-chmod'd merge can produce the
	// 0644 the assertion demands; without it, both routes yield 0644 and the
	// assertion cannot tell them apart.
	defer syscall.Umask(syscall.Umask(0o077))

	// Inputs live outside the output directory so the residue check there is
	// unambiguous.
	srcDir := t.TempDir()
	outDir := t.TempDir()

	a := filepath.Join(srcDir, "a.wav")
	b := filepath.Join(srcDir, "b.wav")
	writeMonoWAV(t, a, sampleRate, sampleRate)
	writeMonoWAV(t, b, sampleRate, sampleRate)

	res, err := MergeMultiChannel(outDir, []MultiChannelInput{
		{LegID: "a", FilePath: a},
		{LegID: "b", FilePath: b, JoinOffset: 500 * time.Millisecond},
	}, time.Second, sampleRate)
	if err != nil {
		t.Fatalf("MergeMultiChannel: %v", err)
	}

	assertPlayable(t, res.FilePath, 2)
	assertPublishedMode(t, res.FilePath)
	assertNoStagingResidue(t, outDir)
}

// TestMergeMultiChannel_InvalidInputCreatesNoOutput covers the input-validation
// refusal: an unreadable input is rejected before any output file is created, so
// the output directory is left untouched.
//
// Note what this does NOT cover. The merge returns here at the input-validation
// loop, before staging has begun, so it never reaches the failure cleanup and
// that cleanup is not what these assertions observe. Exercising it would need an
// output write to fail, and the merge opens its own output, so there is no
// injection point for one.
func TestMergeMultiChannel_InvalidInputCreatesNoOutput(t *testing.T) {
	srcDir := t.TempDir()
	outDir := t.TempDir()

	bad := filepath.Join(srcDir, "bad.wav")
	if err := os.WriteFile(bad, []byte("not a wav"), 0o644); err != nil {
		t.Fatalf("write %s: %v", bad, err)
	}

	if _, err := MergeMultiChannel(outDir, []MultiChannelInput{
		{LegID: "a", FilePath: bad},
	}, time.Second, 16000); err == nil {
		t.Fatal("MergeMultiChannel accepted an invalid input WAV, want error")
	}
	assertNoStagingResidue(t, outDir)

	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatalf("read %s: %v", outDir, err)
	}
	if len(entries) != 0 {
		t.Errorf("a failed merge left %d files in %s, want none", len(entries), outDir)
	}
}
