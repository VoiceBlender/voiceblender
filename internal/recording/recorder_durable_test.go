package recording

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-audio/audio"
	"github.com/go-audio/wav"
)

// stagingFiles lists the staging files sitting in dir.
func stagingFiles(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, stagingPattern))
	if err != nil {
		t.Fatalf("glob staging files in %s: %v", dir, err)
	}
	return m
}

// assertNoStagingResidue fails if a staging file outlived the recording.
func assertNoStagingResidue(t *testing.T, dir string) {
	t.Helper()
	if got := stagingFiles(t, dir); len(got) != 0 {
		t.Errorf("staging residue left behind in %s: %v", dir, got)
	}
}

// assertPublishedMode fails unless path carries the mode recordings have always
// been published with. Staging files are opened 0600 and a rename keeps the
// inode's mode, so a recording published without an explicit chmod would
// silently become owner-only and lock out consumers running as another user.
func assertPublishedMode(t *testing.T, path string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := fi.Mode().Perm(); got != recordingFileMode {
		t.Errorf("%s published with mode %v, want %v", path, got, os.FileMode(recordingFileMode))
	}
}

// assertPlayable fails unless path holds a WAV the standard decoder accepts,
// which is what proves the size header was rewritten before publishing.
func assertPlayable(t *testing.T, path string, wantChannels int) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	dec := wav.NewDecoder(f)
	if !dec.IsValidFile() {
		t.Fatalf("%s is not a valid WAV — it was published before its header was final", path)
	}
	if got := int(dec.NumChans); got != wantChannels {
		t.Errorf("%s has %d channels, want %d", path, got, wantChannels)
	}
	buf, err := dec.FullPCMBuffer()
	if err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	if len(buf.Data) == 0 {
		t.Errorf("%s decoded to no audio", path)
	}
}

func TestPublishFile(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "publish.wav")

	staged, err := createStagedFile(final)
	if err != nil {
		t.Fatalf("createStagedFile: %v", err)
	}
	want := []byte("durable bytes")
	if _, err := staged.f.Write(want); err != nil {
		t.Fatalf("write staged: %v", err)
	}

	// Nothing is at the final name until it is published.
	if _, err := os.Stat(final); !os.IsNotExist(err) {
		t.Fatalf("%s exists before publish, os.Stat err = %v", final, err)
	}

	if err := publishFile(staged.f, staged.tmpPath, staged.finalPath); err != nil {
		t.Fatalf("publishFile: %v", err)
	}

	got, err := os.ReadFile(final)
	if err != nil {
		t.Fatalf("read published file: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("published content = %q, want %q", got, want)
	}
	if _, err := os.Stat(staged.tmpPath); !os.IsNotExist(err) {
		t.Errorf("staging file %s survived publish, os.Stat err = %v", staged.tmpPath, err)
	}
	assertPublishedMode(t, final)
	assertNoStagingResidue(t, dir)
}

func TestPublishFile_RenameFailureLeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	staged, err := createStagedFile(filepath.Join(dir, "unreachable.wav"))
	if err != nil {
		t.Fatalf("createStagedFile: %v", err)
	}
	if _, err := staged.f.Write([]byte("bytes")); err != nil {
		t.Fatalf("write staged: %v", err)
	}

	// A directory that does not exist cannot be renamed into.
	bad := filepath.Join(dir, "absent", "publish.wav")
	if err := publishFile(staged.f, staged.tmpPath, bad); err == nil {
		t.Fatal("publishFile succeeded renaming into a missing directory, want error")
	}
	if _, err := os.Stat(bad); !os.IsNotExist(err) {
		t.Errorf("%s exists after a failed publish, os.Stat err = %v", bad, err)
	}
	assertNoStagingResidue(t, dir)
}

func TestDiscardTemp(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "discarded.wav")

	staged, err := createStagedFile(final)
	if err != nil {
		t.Fatalf("createStagedFile: %v", err)
	}
	if _, err := staged.f.Write([]byte("bytes that must not surface")); err != nil {
		t.Fatalf("write staged: %v", err)
	}

	discardTemp(staged.f, staged.tmpPath)

	if _, err := os.Stat(final); !os.IsNotExist(err) {
		t.Errorf("%s exists after discard, os.Stat err = %v", final, err)
	}
	if _, err := os.Stat(staged.tmpPath); !os.IsNotExist(err) {
		t.Errorf("staging file %s survived discard, os.Stat err = %v", staged.tmpPath, err)
	}
	assertNoStagingResidue(t, dir)
}

func TestSyncDir(t *testing.T) {
	if err := syncDir(t.TempDir()); err != nil {
		t.Errorf("syncDir: %v", err)
	}
	if err := syncDir(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("syncDir on a missing directory succeeded, want error")
	}
}

// frameGate wraps the recorder's input and reports when the recorder has come
// back for a second read. That is the moment the first frame has provably been
// through enc.Write, so the mid-flight assertions below describe a recording
// that is genuinely capturing audio — established without a sleep, which would
// make the test both flaky and vacuous.
//
// It is read only by the recording goroutine; the test only ever receives on
// secondRead.
type frameGate struct {
	r          *syncPipeReader
	reads      int
	secondRead chan struct{}
}

func (g *frameGate) Read(p []byte) (int, error) {
	g.reads++
	if g.reads == 2 {
		close(g.secondRead)
	}
	return g.r.Read(p)
}

// TestRecorder_StartStop_PublishesFinalOnly is the headline guard: a recording
// in progress must exist only as a staging file, and its published name must
// stay absent until the recording stops. Without that, anything listing the
// directory mid-call can pick up a WAV whose size header has not been written
// yet.
//
// The mid-flight assertion is the load-bearing one. The post-stop state is
// identical whether or not the recording was staged, so post-stop checks alone
// would pass against a recorder that wrote straight to its final name.
func TestRecorder_StartStop_PublishesFinalOnly(t *testing.T) {
	const sampleRate = 8000

	dir := t.TempDir()
	r := NewRecorder(slog.Default())

	pr, pw := newSyncPipe()
	gate := &frameGate{r: pr, secondRead: make(chan struct{})}

	fpath, err := r.StartAt(context.Background(), gate, dir, sampleRate)
	if err != nil {
		t.Fatalf("StartAt: %v", err)
	}

	// One 20 ms frame of audible audio.
	frame := make([]byte, 640)
	for i := range frame {
		frame[i] = 0x11
	}
	if _, err := pw.Write(frame); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Mid-flight: the frame is encoded and the recorder is parked on the next read.
	<-gate.secondRead

	if got := stagingFiles(t, dir); len(got) != 1 {
		t.Fatalf("mid-recording: found %d staging files in %s (%v), want exactly 1 — the recording is not being staged", len(got), dir, got)
	}
	if _, err := os.Stat(fpath); !os.IsNotExist(err) {
		t.Fatalf("mid-recording: %s already exists, os.Stat err = %v — a partial recording is visible at its published name", fpath, err)
	}
	if r.Published() {
		t.Error("mid-recording: Published() is true before the recording stopped")
	}

	pw.Close()
	if got := r.Stop(); got != fpath {
		t.Errorf("Stop returned %q, want the path StartAt returned, %q", got, fpath)
	}
	r.Wait()

	// Post-stop: the recording is at its published name, whole, and nothing else is left.
	assertPlayable(t, fpath, 1)
	assertPublishedMode(t, fpath)
	assertNoStagingResidue(t, dir)
	if !r.Published() {
		t.Error("Published() is false after a recording that stopped normally")
	}
}

// TestRecorder_CaptureError_DiscardsStagedFile covers the discard branch: a
// capture that fails must leave nothing at its published name.
//
// The error is raised by handing recordStereo a companion it cannot drain
// without blocking, which is a real, reachable capture failure. A true
// enc.Write failure needs the underlying file write to fail and is not
// practically inducible here; it reaches this same branch, which is also
// covered directly by TestDiscardTemp.
func TestRecorder_CaptureError_DiscardsStagedFile(t *testing.T) {
	dir := t.TempDir()
	r := NewRecorder(slog.Default())

	fpath, err := r.StartStereo(context.Background(), &blockingReader{}, &blockingReader{}, dir, 8000)
	if err != nil {
		t.Fatalf("StartStereo: %v", err)
	}
	r.Wait()

	if _, err := os.Stat(fpath); !os.IsNotExist(err) {
		t.Errorf("%s exists after a failed capture, os.Stat err = %v", fpath, err)
	}
	if r.Published() {
		t.Error("Published() is true after a capture that failed and was discarded")
	}
	assertNoStagingResidue(t, dir)
}

// eofReader yields no data and ends immediately, so a capture loop reading it
// writes nothing and sees no error of its own.
type eofReader struct{}

func (eofReader) Read(p []byte) (int, error) { return 0, io.EOF }

// TestRecorder_CloseErrorIsCaptureFailure pins the one capture failure the loop
// itself cannot see. go-audio's Encoder.Close is the sole writer of the real
// RIFF/data sizes, so if it fails the file keeps its placeholder header and is
// not a playable WAV — yet every write the loop made succeeded. Closing the fd
// out from under the encoder reproduces exactly that.
//
// What this pins is that a Close failure is surfaced as a capture error at all,
// which is what matters for a capture that wrote frames and then failed its
// header rewrite: nothing else would catch it. This case reaches the same
// discard by a second route — the reader ends at once, so enc.Write never runs
// and the zero-frame guard in finish() would discard the file regardless.
func TestRecorder_CloseErrorIsCaptureFailure(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "closed.wav"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Close the fd so the encoder's trailing header rewrite cannot land.
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	r := NewRecorder(slog.Default())
	if _, err := r.recordMono(context.Background(), eofReader{}, f, 8000); err == nil {
		t.Fatal("recordMono reported success though the WAV size header could not be rewritten")
	}
}

// cancelOnlyReader hands over exactly one frame and then never ends: every
// later read returns (0, nil), never data, never an error, never EOF. Nothing
// about it is cancellable, so the capture loop just keeps cycling and the
// context cancel is the only exit that exists — which is what makes it a
// faithful probe of the cancel path. The sleep paces the loop instead of
// spinning hot; it costs no determinism, because the first iteration after Stop
// hits the ctx.Done select regardless.
//
// It is only ever read by the recording goroutine.
type cancelOnlyReader struct {
	first chan struct{}
	sent  bool
}

func (c *cancelOnlyReader) Read(p []byte) (int, error) {
	if !c.sent {
		c.sent = true
		n := copy(p, bytes.Repeat([]byte{0x11}, 640))
		close(c.first)
		return n, nil
	}
	time.Sleep(time.Millisecond)
	return 0, nil
}

// TestRecorder_NormalStopPublishes pins the finalize trigger: Stop cancels the
// recording's context, and a cancelled context is the normal end of a
// recording, not a failure. Treating it as one would discard every recording
// that was stopped the ordinary way.
//
// The reader deliberately never ends on its own. An input that ends by itself —
// a closed pipe or a drained buffer — lets the capture loop exit through its EOF
// path, which publishes too, so the test would pass without the cancel path ever
// working. Here the cancel is the only way out.
func TestRecorder_NormalStopPublishes(t *testing.T) {
	dir := t.TempDir()
	r := NewRecorder(slog.Default())

	rd := &cancelOnlyReader{first: make(chan struct{})}
	fpath, err := r.StartAt(context.Background(), rd, dir, 8000)
	if err != nil {
		t.Fatalf("StartAt: %v", err)
	}

	// One frame is provably in before the recording is stopped.
	<-rd.first

	r.Stop()
	r.Wait()

	if !r.Published() {
		t.Fatal("Published() is false after Stop — a normally stopped recording was discarded")
	}
	assertPlayable(t, fpath, 1)
	assertNoStagingResidue(t, dir)
}

// assertDiscarded fails unless a capture left nothing behind: nothing at its
// published name, no staging residue, and Published() reporting false. That is
// the whole contract a zero-frame capture must meet, so a future change that
// publishes an unreadable file or leaks a staging file is caught here.
func assertDiscarded(t *testing.T, r *Recorder, dir, fpath string) {
	t.Helper()
	if r.Published() {
		t.Errorf("Published() is true after a capture that wrote no frames — %s is not a readable WAV", fpath)
	}
	if _, err := os.Stat(fpath); !os.IsNotExist(err) {
		t.Errorf("%s exists after a zero-frame capture, os.Stat err = %v", fpath, err)
	}
	assertNoStagingResidue(t, dir)
}

// TestRecorder_ZeroFrameCaptureIsNotPublished pins the mono zero-frame guard.
// The encoder writes the RIFF header on its first write, so a capture whose
// reader ends before a single frame arrives leaves a headerless file that no
// decoder can open — and Close reports no error for it, so the capture looks
// successful. Publishing it would report success for a file nothing can read
// and hand the multi-channel merge an input it fails on, taking every other
// participant's audio down with it.
func TestRecorder_ZeroFrameCaptureIsNotPublished(t *testing.T) {
	dir := t.TempDir()
	r := NewRecorder(slog.Default())

	fpath, err := r.StartAt(context.Background(), eofReader{}, dir, 8000)
	if err != nil {
		t.Fatalf("StartAt: %v", err)
	}
	r.Wait()

	assertDiscarded(t, r, dir, fpath)
}

// TestRecorder_StopBeforeFirstFrameIsNotPublished covers the reachable shape:
// the capture is stopped before it ever reads. A participant who joins and
// leaves inside one 20 ms mix tick gets exactly this, because the loop checks
// its context before its first read.
//
// The frame is queued in the pipe before the capture starts, so this also pins
// what the discard decision rests on: audio that was genuinely ready is dropped
// on this path. That is why a zero-frame leg is omitted and named rather than
// given a silent channel — a silent channel would assert this participant said
// nothing, when in fact their audio was discarded.
func TestRecorder_StopBeforeFirstFrameIsNotPublished(t *testing.T) {
	dir := t.TempDir()
	r := NewRecorder(slog.Default())

	// One full frame of audible audio, queued and ready before the capture runs.
	pr, pw := newSyncPipe()
	if _, err := pw.Write(bytes.Repeat([]byte{0x11}, 640)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Already cancelled: the loop's ctx check precedes its first read, so the
	// queued frame is never read and no frame reaches the encoder.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	fpath, err := r.StartAt(ctx, pr, dir, 8000)
	if err != nil {
		t.Fatalf("StartAt: %v", err)
	}
	r.Wait()

	assertDiscarded(t, r, dir, fpath)
}

// TestRecorder_ZeroFrameStereoCaptureIsNotPublished pins the same guard on the
// stereo loop, which has its own encoder and its own early returns. The master
// clock EOFs before clocking a single slot, so no slot is ever written.
func TestRecorder_ZeroFrameStereoCaptureIsNotPublished(t *testing.T) {
	dir := t.TempDir()
	r := NewRecorder(slog.Default())

	leftPR, _ := newSyncPipe()
	fpath, err := r.StartStereo(context.Background(), leftPR, bytes.NewReader(nil), dir, 8000)
	if err != nil {
		t.Fatalf("StartStereo: %v", err)
	}
	r.Wait()

	assertDiscarded(t, r, dir, fpath)
}

// closeAfterFrameReader hands over one full frame and then closes the file the
// encoder writes to before ending the capture. The frame's write lands on a
// healthy fd and only the encoder's trailing header rewrite fails, which is the
// shape of a disk that fills after a call has been recording for a while.
//
// It is only ever read by the capture loop.
type closeAfterFrameReader struct {
	f    *os.File
	sent bool
}

func (c *closeAfterFrameReader) Read(p []byte) (int, error) {
	if !c.sent {
		c.sent = true
		return copy(p, bytes.Repeat([]byte{0x11}, 640)), nil
	}
	c.f.Close()
	return 0, io.EOF
}

// TestRecorder_CloseErrorAfterFramesIsCaptureError pins the pair that
// TestRecorder_HeaderRewriteFailureIsDiscarded then feeds to finish: a capture
// can report wrote==true and an error at the same time. Nothing else in the
// suite produces that combination — the other close-failure case never reaches
// its first write — so without this the discard gate's error half would look
// like a state the capture loops cannot actually reach.
func TestRecorder_CloseErrorAfterFramesIsCaptureError(t *testing.T) {
	staged, err := createStagedFile(filepath.Join(t.TempDir(), "rewrite.wav"))
	if err != nil {
		t.Fatalf("createStagedFile: %v", err)
	}
	defer os.Remove(staged.tmpPath)

	r := NewRecorder(slog.Default())
	wrote, err := r.recordMono(context.Background(), &closeAfterFrameReader{f: staged.f}, staged.f, 8000)
	if !wrote {
		t.Error("recordMono reported no frames though one was encoded before the failure")
	}
	if err == nil {
		t.Error("recordMono reported success though the WAV size header could not be rewritten")
	}
}

// decodedSamples reports how many samples a decoder reads out of path. That is
// what the WAV's size header claims, which is not the same as what was written
// to it when the header rewrite never ran.
func decodedSamples(t *testing.T, path string) int {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	buf, err := wav.NewDecoder(f).FullPCMBuffer()
	if err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return len(buf.Data)
}

// TestRecorder_HeaderRewriteFailureIsDiscarded pins the discard gate's error
// half for a capture that did write frames. A leg records normally, then the
// disk fills and enc.Close — the sole writer of the real RIFF/data sizes —
// fails. Every frame is on disk and the fd is healthy, so nothing downstream of
// the gate would object: the sync, chmod and rename all succeed and the file
// lands at its published name. The capture error is the only thing that knows
// the header is still a placeholder.
//
// The staged file is built the way a failed Close leaves one — frames encoded,
// enc.Close deliberately not run — so the fd handed to finish is genuinely
// publishable and the gate is the only thing standing between it and the
// published name. A staging file broken by closing its fd would not test the
// gate at all: publishFile would fail on its own and discard the file anyway.
func TestRecorder_HeaderRewriteFailureIsDiscarded(t *testing.T) {
	const frameSamples = 320

	dir := t.TempDir()
	fpath := filepath.Join(dir, "rewrite-failed.wav")

	staged, err := createStagedFile(fpath)
	if err != nil {
		t.Fatalf("createStagedFile: %v", err)
	}

	enc := wav.NewEncoder(staged.f, 8000, 16, 1, 1)
	if err := enc.Write(&audio.IntBuffer{
		Format: &audio.Format{SampleRate: 8000, NumChannels: 1},
		Data:   make([]int, frameSamples),
	}); err != nil {
		t.Fatalf("encode frame: %v", err)
	}

	// The harm the gate prevents, stated as an assertion: the file still decodes,
	// so no consumer errors on it — it just silently yields a fraction of the
	// audio the capture actually wrote.
	if got := decodedSamples(t, staged.tmpPath); got >= frameSamples {
		t.Fatalf("staging file decodes to %d of the %d samples written: its header is not a "+
			"placeholder, so this test no longer reproduces a failed header rewrite", got, frameSamples)
	}

	r := NewRecorder(slog.Default())
	r.finish(staged, true, fmt.Errorf("close recording: %w", os.ErrClosed))

	if _, err := os.Stat(fpath); !os.IsNotExist(err) {
		t.Errorf("%s exists after a capture whose header rewrite failed, os.Stat err = %v — "+
			"a recording nothing can fully read was published at its advertised path", fpath, err)
	}
	if r.Published() {
		t.Error("Published() is true after a capture whose header rewrite failed")
	}
	assertNoStagingResidue(t, dir)
}
