package recording

import (
	"bytes"
	"context"
	"encoding/binary"
	"log/slog"
	"os"
	"testing"
	"time"
)

const (
	monoRate        = 8000
	monoSlotBytes   = monoRate / 50 * 2 // one 20 ms tap frame
	monoSlotSamples = monoSlotBytes / 2
)

// readMonoWAV returns a mono WAV's PCM samples.
func readMonoWAV(t *testing.T, path string) []int16 {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(data) < 44 {
		t.Fatalf("WAV too small: %d bytes", len(data))
	}
	pcm := data[44:] // skip the WAV header
	out := make([]int16, 0, len(pcm)/2)
	for i := 0; i+1 < len(pcm); i += 2 {
		out = append(out, int16(binary.LittleEndian.Uint16(pcm[i:])))
	}
	return out
}

// runMono records src through nSlots manual ticks and returns the samples. The
// tick channel is unbuffered, so every send lands only once the previous slot
// has been fully written.
func runMono(t *testing.T, rate, nSlots int, src *scriptedChannel) []int16 {
	t.Helper()
	dir := t.TempDir()
	rec := NewRecorder(slog.Default())
	tickCh := make(chan time.Time)
	rec.newTicker = func() (<-chan time.Time, func()) { return tickCh, func() {} }

	fpath, err := rec.StartAt(context.Background(), src, dir, uint32(rate), "")
	if err != nil {
		t.Fatalf("StartAt: %v", err)
	}
	for k := 0; k < nSlots; k++ {
		tickCh <- time.Time{}
	}
	rec.Stop()
	rec.Wait()

	assertNoStagingResidue(t, dir)
	assertPublishedMode(t, fpath)

	return readMonoWAV(t, fpath)
}

func monoVal(k int) int16 { return int16(1000 + k) }

// TestRecordMono_GapPreservesTimeline is the headline guard: a producer that
// stops arriving mid-capture — a held SIP leg, a SIPREC stream whose sender goes
// quiet — must be silence-filled, and the audio after the gap must stay at its
// true offset instead of sliding earlier and shortening the file.
func TestRecordMono_GapPreservesTimeline(t *testing.T) {
	const (
		nSlots   = 30
		stallAt  = 8
		resumeAt = 20
	)

	src := newScriptedChannel(perSlot(nSlots, monoSlotBytes, func(k int) (int16, bool) {
		return monoVal(k), k < stallAt || k >= resumeAt
	}))

	got := runMono(t, monoRate, nSlots, src)

	if want := nSlots * monoSlotSamples; len(got) != want {
		t.Fatalf("recorded %d samples, want %d — a stalled producer must not shorten the recording", len(got), want)
	}
	for slot := 0; slot < nSlots; slot++ {
		want := monoVal(slot)
		if slot >= stallAt && slot < resumeAt {
			want = 0
		}
		base := slot * monoSlotSamples
		for i := base; i < base+monoSlotSamples; i++ {
			if got[i] != want {
				t.Fatalf("sample[%d] (slot %d) = %d, want %d — audio drifted across the gap", i, slot, got[i], want)
			}
		}
	}
}

// TestRecordMono_StaysAligned proves a bursting producer maps onto slots in
// order rather than being written out all at once.
func TestRecordMono_StaysAligned(t *testing.T) {
	const (
		nSlots = 20
		nBurst = 8 // ahead of the clock, but within the accumulator bound
	)

	slots := make([][]byte, nSlots)
	slots[0] = burst(nBurst, monoSlotBytes, monoVal)

	got := runMono(t, monoRate, nSlots, newScriptedChannel(slots))

	if want := nSlots * monoSlotSamples; len(got) != want {
		t.Fatalf("recorded %d samples, want %d", len(got), want)
	}
	for slot := 0; slot < nSlots; slot++ {
		want := int16(0)
		if slot < nBurst {
			want = monoVal(slot)
		}
		base := slot * monoSlotSamples
		for i := base; i < base+monoSlotSamples; i++ {
			if got[i] != want {
				t.Fatalf("sample[%d] (slot %d) = %d, want %d — burst drifted off the clock", i, slot, got[i], want)
			}
		}
	}
}

// TestRecordMono_Pause_WritesSilence mirrors TestRecorder_Pause_WritesSilence on
// the clocked path: pausing silences the audio without shortening the timeline.
func TestRecordMono_Pause_WritesSilence(t *testing.T) {
	const (
		nSlots   = 15
		pauseAt  = 5
		resumeAt = 10
	)

	rec := NewRecorder(slog.Default())
	src := newScriptedChannel(perSlot(nSlots, monoSlotBytes, func(k int) (int16, bool) {
		return monoVal(k), true
	}))
	// The hooks run at the start of slot k's drain — after slot k-1 was written
	// and before slot k is — so they land exactly on a slot boundary.
	src.hooks = map[int]func(){
		pauseAt: func() {
			if !rec.Pause() {
				t.Errorf("Pause() = false, want true")
			}
		},
		resumeAt: func() {
			if !rec.Resume() {
				t.Errorf("Resume() = false, want true")
			}
		},
	}

	dir := t.TempDir()
	tickCh := make(chan time.Time)
	rec.newTicker = func() (<-chan time.Time, func()) { return tickCh, func() {} }
	fpath, err := rec.StartAt(context.Background(), src, dir, monoRate, "")
	if err != nil {
		t.Fatalf("StartAt: %v", err)
	}
	for k := 0; k < nSlots; k++ {
		tickCh <- time.Time{}
	}
	rec.Stop()
	rec.Wait()
	assertNoStagingResidue(t, dir)

	got := readMonoWAV(t, fpath)
	if want := nSlots * monoSlotSamples; len(got) != want {
		t.Fatalf("recorded %d samples, want %d — pause must not shorten the recording", len(got), want)
	}
	for slot := 0; slot < nSlots; slot++ {
		paused := slot >= pauseAt && slot < resumeAt
		want := monoVal(slot)
		if paused {
			want = 0
		}
		base := slot * monoSlotSamples
		for i := base; i < base+monoSlotSamples; i++ {
			if got[i] != want {
				t.Fatalf("sample[%d] (slot %d, paused=%v) = %d, want %d", i, slot, paused, got[i], want)
			}
		}
	}
}

// TestRecordMono_ClosedProducerEndsCapture pins that a closed source still ends
// the capture — unlike the stereo path, a mono recording has no companion
// channel left to record once its only producer is gone. The audio it did
// deliver must survive at its own offsets.
func TestRecordMono_ClosedProducerEndsCapture(t *testing.T) {
	const (
		nSlots  = 20
		closeAt = 6
	)

	src := newScriptedChannel(perSlot(nSlots, monoSlotBytes, func(k int) (int16, bool) {
		return monoVal(k), true
	}))
	src.eofAt = closeAt

	dir := t.TempDir()
	rec := NewRecorder(slog.Default())
	tickCh := make(chan time.Time)
	rec.newTicker = func() (<-chan time.Time, func()) { return tickCh, func() {} }

	fpath, err := rec.StartAt(context.Background(), src, dir, monoRate, "")
	if err != nil {
		t.Fatalf("StartAt: %v", err)
	}
	// The capture ends on its own here, so the ticks cannot be counted out: they
	// run until it does.
	stopTicks := make(chan struct{})
	go func() {
		for {
			select {
			case tickCh <- time.Time{}:
			case <-stopTicks:
				return
			}
		}
	}()

	done := make(chan struct{})
	go func() {
		rec.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Wait() did not return after the producer closed — the capture did not end with it")
	}
	close(stopTicks)

	got := readMonoWAV(t, fpath)
	// The tick that discovers the close still emits its slot, and by then there
	// is nothing left to put in it.
	if want := (closeAt + 1) * monoSlotSamples; len(got) != want {
		t.Fatalf("recorded %d samples, want %d — capture must end with its producer", len(got), want)
	}
	for slot := 0; slot <= closeAt; slot++ {
		want := int16(0)
		if slot < closeAt {
			want = monoVal(slot)
		}
		base := slot * monoSlotSamples
		for i := base; i < base+monoSlotSamples; i++ {
			if got[i] != want {
				t.Fatalf("sample[%d] (slot %d) = %d, want %d", i, slot, got[i], want)
			}
		}
	}
}

// TestRecordMono_NoFramesIsNotPublished pins the leading-silence guard: ticks
// that arrive before the producer has spoken must not be recorded, so a capture
// that never sees audio stays empty and is discarded rather than published as
// silence.
func TestRecordMono_NoFramesIsNotPublished(t *testing.T) {
	dir := t.TempDir()
	rec := NewRecorder(slog.Default())
	tickCh := make(chan time.Time)
	rec.newTicker = func() (<-chan time.Time, func()) { return tickCh, func() {} }

	fpath, err := rec.StartAt(context.Background(), newScriptedChannel(make([][]byte, 10)), dir, monoRate, "")
	if err != nil {
		t.Fatalf("StartAt: %v", err)
	}
	for k := 0; k < 10; k++ {
		tickCh <- time.Time{}
	}
	rec.Stop()
	rec.Wait()

	if rec.Finalized() {
		t.Error("Finalized() = true for a capture that never saw a frame, want false")
	}
	if _, err := os.Stat(fpath); !os.IsNotExist(err) {
		t.Errorf("%s exists, os.Stat err = %v — a silent capture must not be published", fpath, err)
	}
	assertNoStagingResidue(t, dir)
}

// TestRecordMono_ContextCancel_StopsRecording proves the clocked mono loop
// unwinds on teardown, on the real clock rather than a scripted one.
func TestRecordMono_ContextCancel_StopsRecording(t *testing.T) {
	dir := t.TempDir()
	r := NewRecorder(slog.Default())
	pr, pw := newSyncPipe()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, err := r.StartAt(ctx, pr, dir, monoRate, ""); err != nil {
		t.Fatalf("StartAt: %v", err)
	}

	// Let it clock a few slots so cancel lands mid-recording.
	go func() {
		for i := 0; i < 20; i++ {
			pw.Write(pcmFrame(0x2222, monoSlotBytes))
			time.Sleep(2 * time.Millisecond)
		}
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()

	// The producer never closes, so only the cancel check can end the loop.
	// Guard with a timeout: a leaked goroutine must fail this test, not hang the
	// suite.
	done := make(chan struct{})
	go func() {
		r.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Wait() did not return after context cancel — the mono recording goroutine leaked")
	}

	if r.IsRecording() {
		t.Error("IsRecording() = true after context cancel, want false")
	}
}

// TestRecordMono_BlockingReaderStillRecords pins the fallback: a source that
// cannot be drained without blocking is still captured, at its own cadence,
// rather than refused the way the stereo path refuses one.
func TestRecordMono_BlockingReaderStillRecords(t *testing.T) {
	dir := t.TempDir()
	r := NewRecorder(slog.Default())

	fpath, err := r.StartAt(context.Background(), blockingOnly{bytes.NewReader(burst(5, monoSlotBytes, monoVal))}, dir, monoRate, "")
	if err != nil {
		t.Fatalf("StartAt: %v", err)
	}
	// The reader ends itself, so the capture is complete once Wait returns.
	r.Wait()

	if !r.Finalized() {
		t.Fatal("Finalized() = false, want true — a blocking reader must still be recorded")
	}
	got := readMonoWAV(t, fpath)
	if want := 5 * monoSlotSamples; len(got) != want {
		t.Fatalf("recorded %d samples, want %d", len(got), want)
	}
	for slot := 0; slot < 5; slot++ {
		base := slot * monoSlotSamples
		for i := base; i < base+monoSlotSamples; i++ {
			if got[i] != monoVal(slot) {
				t.Fatalf("sample[%d] (slot %d) = %d, want %d", i, slot, got[i], monoVal(slot))
			}
		}
	}
}
