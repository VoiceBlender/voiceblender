package wsmedia

import (
	"bytes"
	"io"
	"testing"
	"time"
)

func TestStreamBufferRoundTrip(t *testing.T) {
	sb := newStreamBuffer(1024, 20, 0)
	in := bytes.Repeat([]byte{0xAB}, 320)
	n, err := sb.Write(in)
	if err != nil || n != 320 {
		t.Fatalf("write: n=%d err=%v", n, err)
	}
	out := make([]byte, 320)
	n, err = sb.Read(out)
	if err != nil || n != 320 {
		t.Fatalf("read: n=%d err=%v", n, err)
	}
	if !bytes.Equal(in, out) {
		t.Fatal("data mismatch")
	}
}

func TestStreamBufferDropsOnOverflow(t *testing.T) {
	sb := newStreamBuffer(640, 20, 0) // 2 frames at 20ms@16kHz
	frame := bytes.Repeat([]byte{0xCD}, 640)
	// First write fits exactly.
	if _, err := sb.Write(frame); err != nil {
		t.Fatalf("write 1: %v", err)
	}
	// Second write would exceed capacity — must be dropped silently.
	if _, err := sb.Write(frame); err != nil {
		t.Fatalf("write 2: %v", err)
	}
	if got := sb.Dropped(); got != 640 {
		t.Fatalf("drops=%d, want 640", got)
	}
	// Read should still see only the first frame's worth.
	out := make([]byte, 640)
	if _, err := sb.Read(out); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(out, frame) {
		t.Fatal("first frame corrupted")
	}
}

func TestStreamBufferCloseUnblocksRead(t *testing.T) {
	sb := newStreamBuffer(1024, 20, 0)
	out := make([]byte, 100)
	done := make(chan error, 1)
	go func() {
		_, err := sb.Read(out)
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	sb.Close()
	select {
	case err := <-done:
		if err != io.EOF {
			t.Fatalf("want io.EOF, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Read did not unblock after Close")
	}
}

func TestStreamBufferPacesReads(t *testing.T) {
	sb := newStreamBuffer(4096, 20, 0)
	frame := bytes.Repeat([]byte{1}, 100)
	for i := 0; i < 3; i++ {
		if _, err := sb.Write(frame); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	out := make([]byte, 100)
	// First read returns immediately; subsequent reads should be paced.
	if _, err := sb.Read(out); err != nil {
		t.Fatalf("read 1: %v", err)
	}
	start := time.Now()
	if _, err := sb.Read(out); err != nil {
		t.Fatalf("read 2: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 15*time.Millisecond {
		t.Fatalf("expected pacing ≥15ms, got %v", elapsed)
	}
}

func TestStreamBufferPlayoutWarmsBeforeFirstRead(t *testing.T) {
	const frameBytes = 640
	sb := newStreamBuffer(4096, 20, 2*frameBytes)
	frame := bytes.Repeat([]byte{0x11}, frameBytes)

	out := make([]byte, frameBytes)
	done := make(chan int, 1)
	go func() {
		n, err := sb.Read(out)
		if err != nil {
			t.Errorf("read: %v", err)
		}
		done <- n
	}()

	if _, err := sb.Write(frame); err != nil {
		t.Fatalf("write 1: %v", err)
	}
	select {
	case <-done:
		t.Fatal("read returned before the playout lead was buffered")
	case <-time.After(50 * time.Millisecond):
	}

	if _, err := sb.Write(frame); err != nil {
		t.Fatalf("write 2: %v", err)
	}
	select {
	case n := <-done:
		if n != frameBytes {
			t.Fatalf("read n=%d, want %d", n, frameBytes)
		}
	case <-time.After(time.Second):
		t.Fatal("read did not release once the lead was buffered")
	}
	if got := sb.Underruns(); got != 0 {
		t.Fatalf("underruns=%d, want 0 for the initial warm-up", got)
	}
}

func TestStreamBufferPlayoutRearmsAfterUnderrun(t *testing.T) {
	const frameBytes = 320
	sb := newStreamBuffer(4096, 0, 2*frameBytes) // no pacing, isolate the lead
	frame := bytes.Repeat([]byte{0x22}, frameBytes)
	out := make([]byte, frameBytes)

	// Warm up, then drain the lead.
	for i := 0; i < 2; i++ {
		if _, err := sb.Write(frame); err != nil {
			t.Fatalf("warm write %d: %v", i, err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := sb.Read(out); err != nil {
			t.Fatalf("warm read %d: %v", i, err)
		}
	}

	// Buffer is empty: the next read must re-arm rather than release on the
	// single frame that arrives first.
	done := make(chan struct{})
	go func() {
		if _, err := sb.Read(out); err != nil {
			t.Errorf("read after underrun: %v", err)
		}
		close(done)
	}()
	// Let the reader park on the empty buffer, so it sees the underrun rather
	// than finding the frame below already waiting for it.
	time.Sleep(30 * time.Millisecond)

	if _, err := sb.Write(frame); err != nil {
		t.Fatalf("write 1: %v", err)
	}
	select {
	case <-done:
		t.Fatal("read released on one frame; the lead was not rebuilt")
	case <-time.After(50 * time.Millisecond):
	}

	if _, err := sb.Write(frame); err != nil {
		t.Fatalf("write 2: %v", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("read did not release after the lead was rebuilt")
	}
	if got := sb.Underruns(); got != 1 {
		t.Fatalf("underruns=%d, want 1", got)
	}
}

func TestStreamBufferPlayoutCloseDuringWarmUpKeepsAudio(t *testing.T) {
	const frameBytes = 320
	sb := newStreamBuffer(4096, 0, 4*frameBytes)
	partial := bytes.Repeat([]byte{0x33}, frameBytes)
	if _, err := sb.Write(partial); err != nil {
		t.Fatalf("write: %v", err)
	}

	out := make([]byte, frameBytes)
	done := make(chan int, 1)
	go func() {
		n, err := sb.Read(out)
		if err != nil {
			t.Errorf("read: %v", err)
		}
		done <- n
	}()
	time.Sleep(20 * time.Millisecond)
	sb.Close()

	select {
	case n := <-done:
		if n != frameBytes {
			t.Fatalf("read n=%d, want the buffered %d bytes back", n, frameBytes)
		}
		if !bytes.Equal(out, partial) {
			t.Fatal("buffered audio discarded on close")
		}
	case <-time.After(time.Second):
		t.Fatal("read did not unblock after Close")
	}
}

func TestStreamBufferPlayoutDisabledIsPassthrough(t *testing.T) {
	sb := newStreamBuffer(4096, 0, 0)
	frame := bytes.Repeat([]byte{0x44}, 320)
	if _, err := sb.Write(frame); err != nil {
		t.Fatalf("write: %v", err)
	}
	out := make([]byte, 320)
	if _, err := sb.Read(out); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(out, frame) {
		t.Fatal("data mismatch")
	}
	if got := sb.Underruns(); got != 0 {
		t.Fatalf("underruns=%d, want 0 when jitter buffering is off", got)
	}
}

func TestStreamBufferPlayoutLargerThanCapacityIsIgnored(t *testing.T) {
	sb := newStreamBuffer(320, 0, 640)
	frame := bytes.Repeat([]byte{0x55}, 320)
	if _, err := sb.Write(frame); err != nil {
		t.Fatalf("write: %v", err)
	}
	out := make([]byte, 320)
	if _, err := sb.Read(out); err != nil {
		t.Fatalf("read: %v", err)
	}
}
