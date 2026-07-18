package api

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/recording"
	"github.com/go-audio/wav"
)

// TestPipeReader_TryRead covers the non-blocking read contract: nothing ready
// yields (0, nil) without waiting, a buffered remainder is served before the
// channel, and io.EOF appears only after Close plus a full drain.
func TestPipeReader_TryRead(t *testing.T) {
	t.Run("empty returns zero without blocking", func(t *testing.T) {
		pr, _ := createPipe()
		p := make([]byte, 4)
		n, err := pr.TryRead(p)
		if n != 0 || err != nil {
			t.Fatalf("TryRead on empty pipe = (%d, %v), want (0, nil)", n, err)
		}
	})

	t.Run("serves a queued frame", func(t *testing.T) {
		pr, pw := createPipe()
		pw.Write([]byte{1, 2, 3, 4})
		p := make([]byte, 4)
		n, err := pr.TryRead(p)
		if n != 4 || err != nil {
			t.Fatalf("TryRead = (%d, %v), want (4, nil)", n, err)
		}
		if string(p) != string([]byte{1, 2, 3, 4}) {
			t.Fatalf("TryRead payload = %v, want [1 2 3 4]", p)
		}
	})

	t.Run("serves buffered remainder before the channel", func(t *testing.T) {
		pr, pw := createPipe()
		pw.Write([]byte{1, 2, 3, 4})
		pw.Write([]byte{9, 9})

		// A short read leaves a remainder buffered on the reader.
		small := make([]byte, 2)
		n, err := pr.TryRead(small)
		if n != 2 || err != nil {
			t.Fatalf("first TryRead = (%d, %v), want (2, nil)", n, err)
		}
		if small[0] != 1 || small[1] != 2 {
			t.Fatalf("first TryRead payload = %v, want [1 2]", small)
		}

		// The remainder of frame one must win over the queued frame two.
		n, err = pr.TryRead(small)
		if n != 2 || err != nil {
			t.Fatalf("second TryRead = (%d, %v), want (2, nil)", n, err)
		}
		if small[0] != 3 || small[1] != 4 {
			t.Fatalf("remainder served out of order: got %v, want [3 4]", small)
		}

		n, err = pr.TryRead(small)
		if n != 2 || err != nil {
			t.Fatalf("third TryRead = (%d, %v), want (2, nil)", n, err)
		}
		if small[0] != 9 || small[1] != 9 {
			t.Fatalf("third TryRead payload = %v, want [9 9]", small)
		}
	})

	t.Run("EOF only after close and drain", func(t *testing.T) {
		pr, pw := createPipe()
		pw.Write([]byte{7, 7})
		pw.Close()

		// Still queued data: must be served, not swallowed by the close.
		p := make([]byte, 4)
		n, err := pr.TryRead(p)
		if n != 2 || err != nil {
			t.Fatalf("TryRead after Close with data queued = (%d, %v), want (2, nil)", n, err)
		}

		n, err = pr.TryRead(p)
		if n != 0 || err != io.EOF {
			t.Fatalf("TryRead after Close and drain = (%d, %v), want (0, io.EOF)", n, err)
		}
	})

	t.Run("open and empty is not EOF", func(t *testing.T) {
		pr, _ := createPipe()
		p := make([]byte, 4)
		for i := 0; i < 3; i++ {
			n, err := pr.TryRead(p)
			if n != 0 || err != nil {
				t.Fatalf("TryRead on open empty pipe = (%d, %v), want (0, nil)", n, err)
			}
		}
	})
}

// masterGate wraps the master pipe and reports once the recorder has read every
// frame the test queued into it. That is a direct observation of the recorder
// having consumed the pipe, which is what makes closing the writers safe:
// pipeReader.Read selects between a queued frame and the closed-writer signal,
// and Go picks uniformly at random when both are ready, so a close that
// overtakes queued frames discards them and ends the capture early.
//
// It delegates to the real pipeReader, and the companion pipe — the linkage
// under test — is handed to the recorder unwrapped.
//
// It is read only by the recording goroutine; the test only receives on drained.
type masterGate struct {
	r       *pipeReader
	want    int
	read    int
	sent    bool
	drained chan struct{}
}

func (g *masterGate) Read(p []byte) (int, error) {
	n, err := g.r.Read(p)
	g.read += n
	if !g.sent && g.read >= g.want {
		g.sent = true
		close(g.drained)
	}
	return n, err
}

// TestStartStereo_CompanionAudioReachesLeftChannel wires the real recording
// pipes into the real stereo recorder, which is the linkage the recording
// package's own tests cannot cover: they stand in their own pipe double, so
// they would stay green if this pipe stopped satisfying the recorder's
// non-blocking-read requirement and every recording's left channel silently
// went mute.
//
// Every companion frame carries its own marker, so the left channel is checked
// slot by slot for order and completeness rather than for the mere presence of
// companion audio. Frames that all looked alike would let a pop that advances by
// the wrong stride — repeating or skipping frames — still produce a left channel
// indistinguishable from a correct one.
func TestStartStereo_CompanionAudioReachesLeftChannel(t *testing.T) {
	const (
		rate         = 8000
		slotBytes    = rate / 50 * 2 // one 20 ms frame, as the taps emit
		slotSamples  = slotBytes / 2
		nFrames      = 8
		masterSample = int16(0x2222)
	)

	compSample := func(k int) int16 { return int16(0x1000 + k) }

	frameOf := func(v int16) []byte {
		b := make([]byte, slotBytes)
		for i := 0; i+1 < len(b); i += 2 {
			binary.LittleEndian.PutUint16(b[i:], uint16(v))
		}
		return b
	}

	dir := t.TempDir()
	// Exactly the wiring doStartRecordLeg uses for a standalone SIP leg.
	leftPR, leftPW := createPipe()
	rightPR, rightPW := createPipe()
	gate := &masterGate{r: rightPR, want: nFrames * slotBytes, drained: make(chan struct{})}

	rec := recording.NewRecorder(slog.Default())
	fpath, err := rec.StartStereo(context.Background(), leftPR, gate, dir, rate)
	if err != nil {
		t.Fatalf("StartStereo: %v", err)
	}

	// Queue every companion frame before the first master frame. The recorder
	// reads the master before it drains the companion, so with no master frame
	// written it cannot have touched the companion pipe yet: all nFrames are
	// queued by the time the first slot drains it, and each slot pops one. That
	// makes the slot-to-frame mapping below exact rather than timing-dependent.
	for k := 0; k < nFrames; k++ {
		leftPW.Write(frameOf(compSample(k)))
	}
	for k := 0; k < nFrames; k++ {
		rightPW.Write(frameOf(masterSample))
	}

	// Every master frame is provably out of the pipe, so the close races nothing.
	<-gate.drained
	leftPW.Close()
	rightPW.Close()
	rec.Wait()

	f, err := os.Open(fpath)
	if err != nil {
		t.Fatalf("open recording: %v — the recorder wrote nothing, so it rejected these pipes", err)
	}
	defer f.Close()

	dec := wav.NewDecoder(f)
	buf, err := dec.FullPCMBuffer()
	if err != nil {
		t.Fatalf("decode recording: %v", err)
	}
	if got := int(dec.NumChans); got != 2 {
		t.Fatalf("recording has %d channels, want 2", got)
	}

	// The master clocks one slot per frame and nothing else ends the capture, so
	// the slot count is exact.
	if got, want := len(buf.Data), nFrames*slotSamples*2; got != want {
		t.Fatalf("recording holds %d interleaved samples, want %d (%d slots of %d)",
			got, want, nFrames, slotSamples)
	}

	for k := 0; k < nFrames; k++ {
		for i := 0; i < slotSamples; i++ {
			j := (k*slotSamples + i) * 2
			if got := int16(buf.Data[j]); got != compSample(k) {
				t.Fatalf("left channel slot %d sample %d = %#04x, want companion frame %d (%#04x): "+
					"the companion frames did not reach the recording one per slot in order",
					k, i, got, k, compSample(k))
			}
			if got := int16(buf.Data[j+1]); got != masterSample {
				t.Fatalf("right channel slot %d sample %d = %#04x, want master (%#04x): "+
					"the paced pipe is not clocking the recorder", k, i, got, masterSample)
			}
		}
	}
}
