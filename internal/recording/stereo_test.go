package recording

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"
)

const (
	stereoRate        = 8000
	stereoSlotBytes   = stereoRate / 50 * 2 // one 20 ms tap frame
	stereoSlotSamples = stereoSlotBytes / 2
)

// scriptedChannel feeds one stereo channel from a per-slot script. Its TryRead
// hands over slot k's bytes during the recorder's k'th drain and then reports
// empty, so a test controls exactly what each slot sees without racing the
// recording goroutine.
//
// hooks[k] runs at the start of slot k's drain — after slot k-1 was written and
// before slot k is, which is what lets a test flip pause state on an exact slot
// boundary.
type scriptedChannel struct {
	slots   [][]byte
	hooks   map[int]func()
	eofAt   int // slot from which the channel reports EOF; -1 to never close
	k       int
	buf     []byte
	entered bool
}

func newScriptedChannel(slots [][]byte) *scriptedChannel {
	return &scriptedChannel{slots: slots, eofAt: -1}
}

// Read satisfies io.Reader for StartStereo's signature. The recording loop only
// ever calls TryRead.
func (s *scriptedChannel) Read(p []byte) (int, error) {
	return 0, fmt.Errorf("scriptedChannel: blocking Read must not be used")
}

func (s *scriptedChannel) TryRead(p []byte) (int, error) {
	if s.eofAt >= 0 && s.k >= s.eofAt && len(s.buf) == 0 {
		return 0, io.EOF
	}
	if len(s.buf) > 0 {
		n := copy(p, s.buf)
		s.buf = s.buf[n:]
		return n, nil
	}
	if !s.entered {
		s.entered = true
		if h, ok := s.hooks[s.k]; ok {
			h()
		}
		if s.k < len(s.slots) && len(s.slots[s.k]) > 0 {
			s.buf = s.slots[s.k]
			n := copy(p, s.buf)
			s.buf = s.buf[n:]
			return n, nil
		}
	}
	// Nothing more this slot: end the drain and advance.
	s.entered = false
	s.k++
	return 0, nil
}

// pcmFrame builds one slotBytes-long slot of little-endian PCM whose every
// sample is v.
func pcmFrame(v int16, slotBytes int) []byte {
	b := make([]byte, slotBytes)
	for i := 0; i+1 < len(b); i += 2 {
		binary.LittleEndian.PutUint16(b[i:], uint16(v))
	}
	return b
}

// perSlot builds an n-slot script, one frame of value fill(k) per slot. A nil
// return from fill leaves that slot empty.
func perSlot(n, slotBytes int, fill func(k int) (int16, bool)) [][]byte {
	out := make([][]byte, n)
	for k := range out {
		if v, ok := fill(k); ok {
			out[k] = pcmFrame(v, slotBytes)
		}
	}
	return out
}

// burst concatenates count frames into a single slot, modelling a producer that
// dumps a backlog between two ticks.
func burst(count, slotBytes int, val func(c int) int16) []byte {
	out := make([]byte, 0, count*slotBytes)
	for c := 0; c < count; c++ {
		out = append(out, pcmFrame(val(c), slotBytes)...)
	}
	return out
}

// readStereoWAV splits a stereo WAV's interleaved PCM into its two channels.
func readStereoWAV(t *testing.T, path string) (left, right []int16) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(data) < 44 {
		t.Fatalf("WAV too small: %d bytes", len(data))
	}
	pcm := data[44:] // skip the WAV header
	for i := 0; i+3 < len(pcm); i += 4 {
		left = append(left, int16(binary.LittleEndian.Uint16(pcm[i:])))
		right = append(right, int16(binary.LittleEndian.Uint16(pcm[i+2:])))
	}
	return left, right
}

// runStereo records left and right through nSlots manual ticks and returns the
// resulting channels. The tick channel is unbuffered, so every send lands only
// once the previous slot has been fully written.
func runStereo(t *testing.T, rate, nSlots int, left, right *scriptedChannel) (l, r []int16) {
	t.Helper()
	dir := t.TempDir()
	rec := NewRecorder(slog.Default())
	tickCh := make(chan time.Time)
	rec.newTicker = func() (<-chan time.Time, func()) { return tickCh, func() {} }

	fpath, err := rec.StartStereo(context.Background(), left, right, dir, uint32(rate))
	if err != nil {
		t.Fatalf("StartStereo: %v", err)
	}
	for k := 0; k < nSlots; k++ {
		tickCh <- time.Time{}
	}
	rec.Stop()
	rec.Wait()

	// Every stereo case also pins the publish contract: the staging file must
	// not outlive the recording, and the published file must keep the mode
	// consumers have always seen it with.
	assertNoStagingResidue(t, dir)
	assertPublishedMode(t, fpath)

	return readStereoWAV(t, fpath)
}

func leftVal(k int) int16  { return int16(-(1000 + k)) }
func rightVal(k int) int16 { return int16(1000 + k) }

// alwaysRight scripts a right channel that speaks a distinct frame every slot.
func alwaysRight(n, slotBytes int) *scriptedChannel {
	return newScriptedChannel(perSlot(n, slotBytes, func(k int) (int16, bool) { return rightVal(k), true }))
}

// TestRecordStereo_SilenceFillOnCompanionStall is the headline guard: when one
// channel stops arriving mid-call, the clock must keep advancing the file and
// that channel must silence-fill rather than stall the recording.
func TestRecordStereo_SilenceFillOnCompanionStall(t *testing.T) {
	const (
		nSlots     = 20
		leftSlots  = 3
		rightSampl = int16(0x2222)
		leftSampl  = int16(0x1111)
	)

	left := newScriptedChannel(perSlot(nSlots, stereoSlotBytes, func(k int) (int16, bool) {
		return leftSampl, k < leftSlots
	}))
	right := newScriptedChannel(perSlot(nSlots, stereoSlotBytes, func(int) (int16, bool) {
		return rightSampl, true
	}))

	l, r := runStereo(t, stereoRate, nSlots, left, right)

	if want := nSlots * stereoSlotSamples; len(r) != want {
		t.Fatalf("recorded %d frames, want %d — the clock did not drive the recording", len(r), want)
	}
	if len(l) != len(r) {
		t.Fatalf("channel lengths differ: left=%d right=%d", len(l), len(r))
	}

	for i, s := range r {
		if s != rightSampl {
			t.Fatalf("right[%d] = %d, want %d — right audio lost during the left stall", i, s, rightSampl)
		}
	}
	for i := 0; i < leftSlots*stereoSlotSamples; i++ {
		if l[i] != leftSampl {
			t.Fatalf("left[%d] = %d, want %d — left audio lost", i, l[i], leftSampl)
		}
	}
	for i := leftSlots * stereoSlotSamples; i < len(l); i++ {
		if l[i] != 0 {
			t.Fatalf("left[%d] = %d, want 0 — a stalled channel must silence-fill", i, l[i])
		}
	}
}

// TestRecordStereo_StalledProducerStillAdvancesTimeline is the reason the loop
// owns its clock. A held SIP leg, an outbound DTMF burst and a deafened room
// participant all stop the out-tap write entirely; borrowing that tap's cadence
// would freeze BOTH channels and lose the stall from the timeline. Here the
// right channel goes silent mid-call while the left keeps talking: the file must
// still cover every slot, and the left audio must stay at its true offsets.
func TestRecordStereo_StalledProducerStillAdvancesTimeline(t *testing.T) {
	const (
		nSlots   = 20
		stallAt  = 6
		resumeAt = 14
	)

	left := newScriptedChannel(perSlot(nSlots, stereoSlotBytes, func(k int) (int16, bool) {
		return leftVal(k), true
	}))
	right := newScriptedChannel(perSlot(nSlots, stereoSlotBytes, func(k int) (int16, bool) {
		return rightVal(k), k < stallAt || k >= resumeAt
	}))

	l, r := runStereo(t, stereoRate, nSlots, left, right)

	if want := nSlots * stereoSlotSamples; len(r) != want {
		t.Fatalf("recorded %d frames, want %d — a stalled producer must not freeze the timeline", len(r), want)
	}

	for slot := 0; slot < nSlots; slot++ {
		stalled := slot >= stallAt && slot < resumeAt
		wantRight := rightVal(slot)
		if stalled {
			wantRight = 0
		}
		base := slot * stereoSlotSamples
		for i := base; i < base+stereoSlotSamples; i++ {
			// The left channel never stalled, so its audio must stay on its own
			// slots — not slide earlier to fill the right channel's gap.
			if l[i] != leftVal(slot) {
				t.Fatalf("left[%d] (slot %d) = %d, want %d — left audio drifted across the right channel's stall",
					i, slot, l[i], leftVal(slot))
			}
			if r[i] != wantRight {
				t.Fatalf("right[%d] (slot %d, stalled=%v) = %d, want %d", i, slot, stalled, r[i], wantRight)
			}
		}
	}
}

// TestRecordStereo_StaysAligned proves a bursting channel maps onto slots in
// order: slot k carries burst frame k, and the other channel is neither dropped
// nor duplicated.
func TestRecordStereo_StaysAligned(t *testing.T) {
	const (
		nSlots  = 20
		nBurst  = 8 // ahead of the clock, but within the accumulator bound
		burstAt = 0
	)

	leftSlots := make([][]byte, nSlots)
	leftSlots[burstAt] = burst(nBurst, stereoSlotBytes, leftVal)

	l, r := runStereo(t, stereoRate, nSlots, newScriptedChannel(leftSlots), alwaysRight(nSlots, stereoSlotBytes))

	if want := nSlots * stereoSlotSamples; len(r) != want {
		t.Fatalf("recorded %d frames, want %d", len(r), want)
	}

	for slot := 0; slot < nSlots; slot++ {
		base := slot * stereoSlotSamples
		want := int16(0)
		if slot < nBurst {
			want = leftVal(slot)
		}
		for i := base; i < base+stereoSlotSamples; i++ {
			if r[i] != rightVal(slot) {
				t.Fatalf("right[%d] (slot %d) = %d, want %d — right frame dropped or duplicated",
					i, slot, r[i], rightVal(slot))
			}
			if l[i] != want {
				t.Fatalf("left[%d] (slot %d) = %d, want %d — burst drifted off the clock", i, slot, l[i], want)
			}
		}
	}
}

// TestRecordStereo_DropOldestBound proves the accumulator is bounded: a channel
// that floods far past the bound loses its oldest audio, and the other channel
// is untouched by the flood.
//
// It runs at every rate the mixer admits (mixer.go's 8000/16000/48000), which is
// what pins the slot size to the sample rate. maxSlotBacklog is a slot COUNT —
// the one quantity in the design that does not scale with slotBytes — so a slot
// size that stops tracking the rate shifts which frames survive the flood, and
// the retained audio no longer starts at firstKept.
func TestRecordStereo_DropOldestBound(t *testing.T) {
	for _, rate := range []int{8000, 16000, 48000} {
		t.Run(rateName(rate), func(t *testing.T) {
			const (
				nSlots = 24
				flood  = 40 // far beyond maxSlotBacklog
			)
			slotBytes := rate / 50 * 2 // one real 20 ms frame at this rate
			slotSamples := slotBytes / 2

			leftSlots := make([][]byte, nSlots)
			leftSlots[0] = burst(flood, slotBytes, leftVal)

			l, r := runStereo(t, rate, nSlots, newScriptedChannel(leftSlots), alwaysRight(nSlots, slotBytes))

			if want := nSlots * slotSamples; len(r) != want {
				t.Fatalf("recorded %d frames, want %d", len(r), want)
			}

			// Only the newest maxSlotBacklog survive the flood; the rest are
			// dropped oldest-first, so the retained audio starts at this frame.
			firstKept := flood - maxSlotBacklog

			for slot := 0; slot < nSlots; slot++ {
				base := slot * slotSamples
				want := int16(0)
				if slot < maxSlotBacklog {
					want = leftVal(firstKept + slot)
				}
				for i := base; i < base+slotSamples; i++ {
					if r[i] != rightVal(slot) {
						t.Fatalf("right[%d] (slot %d) = %d, want %d — the flood disturbed the other channel",
							i, slot, r[i], rightVal(slot))
					}
					if l[i] != want {
						t.Fatalf("left[%d] (slot %d) = %d, want %d — accumulator did not bound at %d slots dropping oldest",
							i, slot, l[i], want, maxSlotBacklog)
					}
				}
			}

			// The dropped frames must be gone entirely, not merely reordered.
			dropped := make(map[int16]bool, firstKept)
			for c := 0; c < firstKept; c++ {
				dropped[leftVal(c)] = true
			}
			for i, s := range l {
				if dropped[s] {
					t.Fatalf("left[%d] = %d — a frame past the bound was retained", i, s)
				}
			}
		})
	}
}

// TestRecordStereo_Pause_ZeroesBothChannels mirrors TestRecorder_Pause_WritesSilence
// for the stereo path: pausing must silence left and right together, and the
// timeline must still cover the paused stretch.
func TestRecordStereo_Pause_ZeroesBothChannels(t *testing.T) {
	const (
		nSlots    = 15
		pauseAt   = 5
		resumeAt  = 10
		leftSampl = int16(0x1111)
	)

	rec := NewRecorder(slog.Default())
	left := newScriptedChannel(perSlot(nSlots, stereoSlotBytes, func(int) (int16, bool) { return leftSampl, true }))
	right := alwaysRight(nSlots, stereoSlotBytes)
	// The hooks run at the start of slot k's drain — after slot k-1 was written
	// and before slot k is — so they land exactly on a slot boundary.
	right.hooks = map[int]func(){
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
	fpath, err := rec.StartStereo(context.Background(), left, right, dir, stereoRate)
	if err != nil {
		t.Fatalf("StartStereo: %v", err)
	}
	for k := 0; k < nSlots; k++ {
		tickCh <- time.Time{}
	}
	rec.Stop()
	rec.Wait()
	assertNoStagingResidue(t, dir)

	l, r := readStereoWAV(t, fpath)
	if want := nSlots * stereoSlotSamples; len(r) != want {
		t.Fatalf("recorded %d frames, want %d — pause must not shorten the recording", len(r), want)
	}

	for slot := 0; slot < nSlots; slot++ {
		paused := slot >= pauseAt && slot < resumeAt
		wantLeft, wantRight := leftSampl, rightVal(slot)
		if paused {
			wantLeft, wantRight = 0, 0
		}
		base := slot * stereoSlotSamples
		for i := base; i < base+stereoSlotSamples; i++ {
			if l[i] != wantLeft {
				t.Fatalf("left[%d] (slot %d, paused=%v) = %d, want %d", i, slot, paused, l[i], wantLeft)
			}
			if r[i] != wantRight {
				t.Fatalf("right[%d] (slot %d, paused=%v) = %d, want %d", i, slot, paused, r[i], wantRight)
			}
		}
	}
}

// TestRecordStereo_ClosedCompanionKeepsRecording pins the central semantic
// change: neither reader ends the recording. A channel that closes mid-call must
// silence-fill for the rest of the timeline rather than cutting the file short,
// which is what the previous lock-step loop did.
func TestRecordStereo_ClosedCompanionKeepsRecording(t *testing.T) {
	const (
		nSlots    = 20
		closeAt   = 5
		leftSampl = int16(0x1111)
	)

	left := newScriptedChannel(perSlot(nSlots, stereoSlotBytes, func(int) (int16, bool) { return leftSampl, true }))
	left.eofAt = closeAt

	l, r := runStereo(t, stereoRate, nSlots, left, alwaysRight(nSlots, stereoSlotBytes))

	if want := nSlots * stereoSlotSamples; len(r) != want {
		t.Fatalf("recorded %d frames, want %d — a closed channel must not end the recording", len(r), want)
	}
	if len(l) != len(r) {
		t.Fatalf("channel lengths differ: left=%d right=%d", len(l), len(r))
	}
	for i, s := range r {
		if want := rightVal(i / stereoSlotSamples); s != want {
			t.Fatalf("right[%d] = %d, want %d — right audio lost when the left closed", i, s, want)
		}
	}
	for i := 0; i < len(l); i++ {
		want := int16(0)
		if i < closeAt*stereoSlotSamples {
			want = leftSampl
		}
		if l[i] != want {
			t.Fatalf("left[%d] = %d, want %d — a closed channel must silence-fill", i, l[i], want)
		}
	}
}

// TestRecordStereo_NoFramesIsNotPublished pins the leading-silence guard: ticks
// that arrive before either producer has spoken must not be recorded, so a
// capture that never sees audio stays empty and is discarded rather than
// published as silence.
func TestRecordStereo_NoFramesIsNotPublished(t *testing.T) {
	dir := t.TempDir()
	rec := NewRecorder(slog.Default())
	tickCh := make(chan time.Time)
	rec.newTicker = func() (<-chan time.Time, func()) { return tickCh, func() {} }

	silent := func() *scriptedChannel { return newScriptedChannel(make([][]byte, 10)) }
	fpath, err := rec.StartStereo(context.Background(), silent(), silent(), dir, stereoRate)
	if err != nil {
		t.Fatalf("StartStereo: %v", err)
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

// TestRecordStereo_ContextCancel_StopsRecording proves the stereo recording
// goroutine unwinds on teardown, on the real clock rather than a scripted one.
func TestRecordStereo_ContextCancel_StopsRecording(t *testing.T) {
	dir := t.TempDir()
	r := NewRecorder(slog.Default())
	leftPR, _ := newSyncPipe()
	rightPR, rightPW := newSyncPipe()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, err := r.StartStereo(ctx, leftPR, rightPR, dir, stereoRate); err != nil {
		t.Fatalf("StartStereo: %v", err)
	}

	// Let it clock a few slots so cancel lands mid-recording.
	go func() {
		for i := 0; i < 20; i++ {
			rightPW.Write(pcmFrame(0x2222, stereoSlotBytes))
			time.Sleep(2 * time.Millisecond)
		}
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()

	// Neither reader ends the loop, so only the cancel check can. Guard with a
	// timeout: a leaked goroutine must fail this test, not hang the suite.
	done := make(chan struct{})
	go func() {
		r.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Wait() did not return after context cancel — the stereo recording goroutine leaked")
	}

	if r.IsRecording() {
		t.Error("IsRecording() = true after context cancel, want false")
	}
}

// TestRecordStereo_RejectsBlockingReader pins the guard that a reader which
// cannot be drained without blocking is refused outright, rather than silently
// recording one channel of a conversation.
func TestRecordStereo_RejectsBlockingReader(t *testing.T) {
	dir := t.TempDir()
	r := NewRecorder(slog.Default())
	rightPR, _ := newSyncPipe()

	if _, err := r.StartStereo(context.Background(), io.LimitReader(nil, 0), rightPR, dir, stereoRate); err != nil {
		t.Fatalf("StartStereo: %v", err)
	}
	r.Wait()
	if r.Finalized() {
		t.Error("Finalized() = true for a rejected reader, want false")
	}
	assertNoStagingResidue(t, dir)
}

func rateName(rate int) string {
	switch rate {
	case 8000:
		return "8kHz"
	case 16000:
		return "16kHz"
	default:
		return "48kHz"
	}
}
