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

// TestStartStereo_ChannelAudioReachesBothSides wires the real recording pipes
// into the real stereo recorder, which is the linkage the recording package's
// own tests cannot cover: they stand in their own pipe double, so they would
// stay green if this pipe stopped satisfying the recorder's non-blocking-read
// requirement and every recording's left channel silently went mute.
//
// Every left frame carries its own marker, so the channel is checked slot by
// slot for order and completeness rather than for the mere presence of audio.
// Frames that all looked alike would let a pop that advances by the wrong
// stride — repeating or skipping frames — still produce a left channel
// indistinguishable from a correct one.
func TestStartStereo_ChannelAudioReachesBothSides(t *testing.T) {
	const (
		rate        = 8000
		slotBytes   = rate / 50 * 2 // one 20 ms frame, as the taps emit
		slotSamples = slotBytes / 2
		nFrames     = 8
		rightSample = int16(0x2222)
	)

	leftSample := func(k int) int16 { return int16(0x1000 + k) }

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

	// Queue everything and close both writers before the recorder starts. The
	// first drain then sees the whole script and both EOFs at once, so the
	// slot-to-frame mapping below is exact rather than timing-dependent.
	for k := 0; k < nFrames; k++ {
		leftPW.Write(frameOf(leftSample(k)))
		rightPW.Write(frameOf(rightSample))
	}
	leftPW.Close()
	rightPW.Close()

	rec := recording.NewRecorder(slog.Default())
	fpath, err := rec.StartStereo(context.Background(), leftPR, rightPR, dir, rate, "")
	if err != nil {
		t.Fatalf("StartStereo: %v", err)
	}
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

	// Both channels closed after exactly nFrames, and the recorder flushes what
	// it buffered before returning, so the slot count is exact.
	if got, want := len(buf.Data), nFrames*slotSamples*2; got != want {
		t.Fatalf("recording holds %d interleaved samples, want %d (%d slots of %d)",
			got, want, nFrames, slotSamples)
	}

	for k := 0; k < nFrames; k++ {
		for i := 0; i < slotSamples; i++ {
			j := (k*slotSamples + i) * 2
			if got := int16(buf.Data[j]); got != leftSample(k) {
				t.Fatalf("left channel slot %d sample %d = %#04x, want frame %d (%#04x): "+
					"the left frames did not reach the recording one per slot in order",
					k, i, got, k, leftSample(k))
			}
			if got := int16(buf.Data[j+1]); got != rightSample {
				t.Fatalf("right channel slot %d sample %d = %#04x, want %#04x: "+
					"the right pipe did not reach the recording", k, i, got, rightSample)
			}
		}
	}
}
