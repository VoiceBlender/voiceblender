package recording

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"os"
	"testing"
)

const (
	stereoRate        = 8000
	stereoSlotBytes   = stereoRate / 50 * 2 // one 20 ms tap frame
	stereoSlotSamples = stereoSlotBytes / 2
)

// scriptedMaster feeds the stereo recorder's master (right) channel from a
// fixed script and EOFs when it runs out.
//
// The recorder reads the master synchronously at the top of every slot, so a
// hook attached to frame k runs at an exact point in the recording: after slot
// k-1 has been written and before slot k is read. That lets a test inject
// companion audio or flip pause state at a precise slot boundary without
// depending on sleeps or scheduler timing.
type scriptedMaster struct {
	frames [][]byte
	hooks  map[int]func()
	i      int
	buf    []byte
}

func (m *scriptedMaster) Read(p []byte) (int, error) {
	if len(m.buf) > 0 {
		n := copy(p, m.buf)
		m.buf = m.buf[n:]
		return n, nil
	}
	if m.i >= len(m.frames) {
		return 0, io.EOF
	}
	if h, ok := m.hooks[m.i]; ok {
		h()
	}
	frame := m.frames[m.i]
	m.i++
	n := copy(p, frame)
	if n < len(frame) {
		m.buf = frame[n:]
	}
	return n, nil
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

// runStereo records a scripted master alongside a companion pipe at the given
// sample rate and returns the resulting channels. Hooks are invoked per master
// frame.
func runStereo(t *testing.T, rate int, frames [][]byte, hook func(k int, leftPW *syncPipeWriter, r *Recorder)) (left, right []int16) {
	t.Helper()
	dir := t.TempDir()
	r := NewRecorder(slog.Default())
	leftPR, leftPW := newSyncPipe()

	hooks := make(map[int]func(), len(frames))
	for k := range frames {
		k := k
		hooks[k] = func() { hook(k, leftPW, r) }
	}
	master := &scriptedMaster{frames: frames, hooks: hooks}

	fpath, err := r.StartStereo(context.Background(), leftPR, master, dir, uint32(rate))
	if err != nil {
		t.Fatalf("StartStereo: %v", err)
	}
	// The master EOFs at the end of the script, which ends the recording.
	r.Wait()

	// Every stereo case also pins the publish contract: the staging file must
	// not outlive the recording, and the published file must keep the mode
	// consumers have always seen it with.
	assertNoStagingResidue(t, dir)
	assertPublishedMode(t, fpath)

	return readStereoWAV(t, fpath)
}

// masterRamp builds n distinct, strictly positive master frames of slotBytes
// each, so that a dropped or duplicated master frame is detectable by value.
func masterRamp(n, slotBytes int) [][]byte {
	frames := make([][]byte, n)
	for k := range frames {
		frames[k] = pcmFrame(masterVal(k), slotBytes)
	}
	return frames
}

func masterVal(k int) int16    { return int16(1000 + k) }
func companionVal(k int) int16 { return int16(-(1000 + k)) }

// TestRecordStereo_SilenceFillOnCompanionStall is the headline guard: when the
// companion stops arriving mid-call, the master must keep clocking the file and
// the companion side must silence-fill rather than stall the recording.
func TestRecordStereo_SilenceFillOnCompanionStall(t *testing.T) {
	const (
		nSlots         = 20
		companionSlots = 3
		masterSample   = int16(0x2222)
		compSample     = int16(0x1111)
	)

	frames := make([][]byte, nSlots)
	for k := range frames {
		frames[k] = pcmFrame(masterSample, stereoSlotBytes)
	}

	left, right := runStereo(t, stereoRate, frames, func(k int, leftPW *syncPipeWriter, _ *Recorder) {
		// The companion delivers a few frames and then goes quiet for good.
		if k < companionSlots {
			leftPW.Write(pcmFrame(compSample, stereoSlotBytes))
		}
	})

	// The master drives the timeline: one slot out per master frame in, so the
	// file covers the whole call rather than stopping when the companion did.
	if want := nSlots * stereoSlotSamples; len(right) != want {
		t.Fatalf("recorded %d frames, want %d — master clock did not drive the recording", len(right), want)
	}
	if len(left) != len(right) {
		t.Fatalf("channel lengths differ: left=%d right=%d", len(left), len(right))
	}

	// The master channel survives the companion stall untouched.
	for i, s := range right {
		if s != masterSample {
			t.Fatalf("right[%d] = %d, want %d — master audio lost during the companion stall", i, s, masterSample)
		}
	}

	// The companion is audible while it is delivering...
	for i := 0; i < companionSlots*stereoSlotSamples; i++ {
		if left[i] != compSample {
			t.Fatalf("left[%d] = %d, want %d — companion audio lost", i, left[i], compSample)
		}
	}
	// ...and silence-filled once it stops, keeping it aligned with the master.
	for i := companionSlots * stereoSlotSamples; i < len(left); i++ {
		if left[i] != 0 {
			t.Fatalf("left[%d] = %d, want 0 — a stalled companion must silence-fill", i, left[i])
		}
	}
}

// TestRecordStereo_StaysAligned proves the companion maps onto master slots in
// order even when it bursts ahead and then pauses: slot k carries companion
// frame k, and the master sequence is neither dropped nor duplicated.
func TestRecordStereo_StaysAligned(t *testing.T) {
	const (
		nSlots = 20
		burst  = 8 // ahead of the master, but within the accumulator bound
	)

	left, right := runStereo(t, stereoRate, masterRamp(nSlots, stereoSlotBytes), func(k int, leftPW *syncPipeWriter, _ *Recorder) {
		// The companion dumps its whole burst before the first master slot,
		// then never speaks again.
		if k == 0 {
			for c := 0; c < burst; c++ {
				leftPW.Write(pcmFrame(companionVal(c), stereoSlotBytes))
			}
		}
	})

	if want := nSlots * stereoSlotSamples; len(right) != want {
		t.Fatalf("recorded %d frames, want %d", len(right), want)
	}

	for slot := 0; slot < nSlots; slot++ {
		base := slot * stereoSlotSamples
		for i := base; i < base+stereoSlotSamples; i++ {
			// The master sequence must appear exactly once, in order.
			if right[i] != masterVal(slot) {
				t.Fatalf("right[%d] (slot %d) = %d, want %d — master frame dropped or duplicated",
					i, slot, right[i], masterVal(slot))
			}

			// The companion rides the master's clock: burst frame k lands on
			// slot k, and the tail silence-fills.
			want := int16(0)
			if slot < burst {
				want = companionVal(slot)
			}
			if left[i] != want {
				t.Fatalf("left[%d] (slot %d) = %d, want %d — companion drifted off the master clock",
					i, slot, left[i], want)
			}
		}
	}
}

// TestRecordStereo_DropOldestBound proves the companion accumulator is bounded:
// a companion that floods far past the bound loses its oldest audio, and the
// master stream is untouched by the flood.
//
// It runs at every rate the mixer admits (mixer.go's 8000/16000/48000), which
// is what pins the slot size to the sample rate. companionMaxSlots is a slot
// COUNT — the one quantity in the design that does not scale with slotBytes —
// so a slot size that stops tracking the rate shifts which companion frames
// survive the flood, and the retained audio no longer starts at firstKept.
func TestRecordStereo_DropOldestBound(t *testing.T) {
	for _, rate := range []int{8000, 16000, 48000} {
		t.Run(rateName(rate), func(t *testing.T) {
			const (
				nSlots = 24
				flood  = 40 // far beyond companionMaxSlots
			)
			slotBytes := rate / 50 * 2 // one real 20 ms frame at this rate
			slotSamples := slotBytes / 2

			left, right := runStereo(t, rate, masterRamp(nSlots, slotBytes), func(k int, leftPW *syncPipeWriter, _ *Recorder) {
				if k == 0 {
					for c := 0; c < flood; c++ {
						leftPW.Write(pcmFrame(companionVal(c), slotBytes))
					}
				}
			})

			if want := nSlots * slotSamples; len(right) != want {
				t.Fatalf("recorded %d frames, want %d", len(right), want)
			}

			// Only the newest companionMaxSlots survive the flood; the rest are
			// dropped oldest-first, so the retained audio starts at this frame.
			firstKept := flood - companionMaxSlots

			for slot := 0; slot < nSlots; slot++ {
				base := slot * slotSamples

				// The flood must not perturb the master stream at all.
				for i := base; i < base+slotSamples; i++ {
					if right[i] != masterVal(slot) {
						t.Fatalf("right[%d] (slot %d) = %d, want %d — companion flood disturbed the master",
							i, slot, right[i], masterVal(slot))
					}
				}

				want := int16(0)
				if slot < companionMaxSlots {
					want = companionVal(firstKept + slot)
				}
				for i := base; i < base+slotSamples; i++ {
					if left[i] != want {
						t.Fatalf("left[%d] (slot %d) = %d, want %d — accumulator did not bound at %d slots dropping oldest",
							i, slot, left[i], want, companionMaxSlots)
					}
				}
			}

			// The dropped frames must be gone entirely, not merely reordered.
			dropped := make(map[int16]bool, firstKept)
			for c := 0; c < firstKept; c++ {
				dropped[companionVal(c)] = true
			}
			for i, s := range left {
				if dropped[s] {
					t.Fatalf("left[%d] = %d — a companion frame past the bound was retained", i, s)
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
		nSlots     = 15
		pauseAt    = 5
		resumeAt   = 10
		compSample = int16(0x1111)
	)

	left, right := runStereo(t, stereoRate, masterRamp(nSlots, stereoSlotBytes), func(k int, leftPW *syncPipeWriter, r *Recorder) {
		// The companion keeps talking throughout, including while paused.
		leftPW.Write(pcmFrame(compSample, stereoSlotBytes))

		// The hook runs after slot k-1 is written and before slot k is read,
		// so these land exactly on a slot boundary.
		switch k {
		case pauseAt:
			if !r.Pause() {
				t.Errorf("Pause() = false, want true")
			}
		case resumeAt:
			if !r.Resume() {
				t.Errorf("Resume() = false, want true")
			}
		}
	})

	// Pausing preserves the timeline rather than shortening the file.
	if want := nSlots * stereoSlotSamples; len(right) != want {
		t.Fatalf("recorded %d frames, want %d — pause must not shorten the recording", len(right), want)
	}

	for slot := 0; slot < nSlots; slot++ {
		paused := slot >= pauseAt && slot < resumeAt
		wantLeft, wantRight := compSample, masterVal(slot)
		if paused {
			wantLeft, wantRight = 0, 0
		}
		base := slot * stereoSlotSamples
		for i := base; i < base+stereoSlotSamples; i++ {
			if left[i] != wantLeft {
				t.Fatalf("left[%d] (slot %d, paused=%v) = %d, want %d", i, slot, paused, left[i], wantLeft)
			}
			if right[i] != wantRight {
				t.Fatalf("right[%d] (slot %d, paused=%v) = %d, want %d", i, slot, paused, right[i], wantRight)
			}
		}
	}
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
