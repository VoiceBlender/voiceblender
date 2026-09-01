package api

import (
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/mixer"
)

const sbTestFrame = 640 // 20ms @ 16kHz, 16-bit mono

func writeFrames(t *testing.T, sb *streamBuffer, n int, fill byte) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := sb.Write(bytes.Repeat([]byte{fill}, sbTestFrame)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
}

func TestStreamBufferReadUnpacedDoesNotPace(t *testing.T) {
	sb := newStreamBuffer()
	writeFrames(t, sb, 5, 0xAB)

	out := make([]byte, sbTestFrame)
	start := time.Now()
	for i := 0; i < 5; i++ {
		n, err := sb.ReadUnpaced(out)
		if err != nil || n != sbTestFrame {
			t.Fatalf("read %d: n=%d err=%v", i, n, err)
		}
	}
	if elapsed := time.Since(start); elapsed > 2*time.Duration(mixer.Ptime)*time.Millisecond {
		t.Fatalf("ReadUnpaced paced itself: 5 buffered frames took %v", elapsed)
	}
}

// The room path hands the buffer to the mixer, whose incoming channel drops
// overflow — Read must keep pacing.
func TestStreamBufferReadStillPaces(t *testing.T) {
	sb := newStreamBuffer()
	writeFrames(t, sb, 3, 0xCD)

	out := make([]byte, sbTestFrame)
	if _, err := sb.Read(out); err != nil {
		t.Fatalf("read 1: %v", err)
	}
	start := time.Now()
	for i := 0; i < 2; i++ {
		if _, err := sb.Read(out); err != nil {
			t.Fatalf("read %d: %v", i+2, err)
		}
	}
	pace := time.Duration(mixer.Ptime) * time.Millisecond
	if elapsed := time.Since(start); elapsed < 2*pace-5*time.Millisecond {
		t.Fatalf("Read stopped pacing: 2 reads took %v, want ~%v", elapsed, 2*pace)
	}
}

func TestStreamBufferReadUnpacedClosed(t *testing.T) {
	sb := newStreamBuffer()
	sb.Close()
	if _, err := sb.ReadUnpaced(make([]byte, sbTestFrame)); err != io.EOF {
		t.Fatalf("drained+closed: want io.EOF, got %v", err)
	}

	sb = newStreamBuffer()
	if _, err := sb.Write(bytes.Repeat([]byte{7}, 100)); err != nil {
		t.Fatalf("write: %v", err)
	}
	sb.Close()
	n, err := sb.ReadUnpaced(make([]byte, sbTestFrame))
	if err != nil || n != 100 {
		t.Fatalf("partial frame at close: n=%d err=%v", n, err)
	}
}

func TestStreamBufferReadUnpacedEmptySlice(t *testing.T) {
	sb := newStreamBuffer()
	n, err := sb.ReadUnpaced(nil)
	if n != 0 || err != nil {
		t.Fatalf("ReadUnpaced(nil): n=%d err=%v", n, err)
	}
}

// Mirrors the leg-agent path: reader → blocking-send channel → 20ms consumer
// (sip_leg.go's writeLoop, which substitutes silence whenever it finds that
// channel empty). Unpaced reads saturate the channel, so a late reader has a
// full 5-frame cushion to be late against; a paced reader is capped at the
// consumer's own rate and can never build one.
func TestStreamBufferReadUnpacedFillsSinkCushion(t *testing.T) {
	sb := newStreamBuffer()
	defer sb.Close()
	writeFrames(t, sb, 20, 0x5A)

	outFrames := make(chan []byte, 5)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		buf := make([]byte, sbTestFrame)
		for {
			n, err := sb.ReadUnpaced(buf)
			if err != nil || n == 0 {
				return
			}
			frame := make([]byte, n)
			copy(frame, buf[:n])
			select {
			case outFrames <- frame:
			case <-stop:
				return
			}
		}
	}()

	deadline := time.After(2 * time.Duration(mixer.Ptime) * time.Millisecond)
	for {
		if len(outFrames) == cap(outFrames) {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("sink cushion only %d/%d frames after 2 frame intervals", len(outFrames), cap(outFrames))
		default:
			time.Sleep(time.Millisecond)
		}
	}
}
