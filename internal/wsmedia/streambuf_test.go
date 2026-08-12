package wsmedia

import (
	"bytes"
	"io"
	"testing"
	"time"
)

func TestStreamBufferRoundTrip(t *testing.T) {
	sb := newStreamBuffer(1024, 20)
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
	sb := newStreamBuffer(640, 20) // 2 frames at 20ms@16kHz
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
	sb := newStreamBuffer(1024, 20)
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
	sb := newStreamBuffer(4096, 20)
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

func TestStreamBufferPlayoutWarmsThenReleases(t *testing.T) {
	const (
		frameBytes   = 320 // 20ms @ 16kHz s16le
		playoutBytes = 640 // 40ms lead
	)
	sb := newStreamBufferPlayout(4096, 20, playoutBytes)
	frameA := bytes.Repeat([]byte{0x11}, frameBytes)
	frameB := bytes.Repeat([]byte{0x22}, frameBytes)
	frameC := bytes.Repeat([]byte{0x33}, frameBytes)

	out := make([]byte, frameBytes)
	// Not warm yet — first paced read must be silence and must not consume.
	n, err := sb.Read(out)
	if err != nil || n != frameBytes {
		t.Fatalf("warm read: n=%d err=%v", n, err)
	}
	if !bytes.Equal(out, make([]byte, frameBytes)) {
		t.Fatal("expected silence while warming")
	}

	if _, err := sb.Write(frameA); err != nil {
		t.Fatalf("write A: %v", err)
	}
	if _, err := sb.Write(frameB); err != nil {
		t.Fatalf("write B: %v", err)
	}
	// Exactly at target — next read releases real audio.
	n, err = sb.Read(out)
	if err != nil || n != frameBytes {
		t.Fatalf("post-warm read: n=%d err=%v", n, err)
	}
	if !bytes.Equal(out, frameA) {
		t.Fatalf("want frame A after warm-up, got %x", out[:8])
	}

	if _, err := sb.Write(frameC); err != nil {
		t.Fatalf("write C: %v", err)
	}
	n, err = sb.Read(out)
	if err != nil || n != frameBytes {
		t.Fatalf("read B: n=%d err=%v", n, err)
	}
	if !bytes.Equal(out, frameB) {
		t.Fatalf("want frame B, got %x", out[:8])
	}
}

func TestStreamBufferPlayoutUnderrunReturnsSilenceWithoutBlocking(t *testing.T) {
	const frameBytes = 320
	sb := newStreamBufferPlayout(4096, 20, frameBytes) // 20ms lead
	frame := bytes.Repeat([]byte{0x44}, frameBytes)
	if _, err := sb.Write(frame); err != nil {
		t.Fatalf("write: %v", err)
	}
	out := make([]byte, frameBytes)
	if _, err := sb.Read(out); err != nil {
		t.Fatalf("warm release: %v", err)
	}
	if !bytes.Equal(out, frame) {
		t.Fatal("expected real frame after warm-up")
	}

	start := time.Now()
	n, err := sb.Read(out)
	elapsed := time.Since(start)
	if err != nil || n != frameBytes {
		t.Fatalf("underrun read: n=%d err=%v", n, err)
	}
	if !bytes.Equal(out, make([]byte, frameBytes)) {
		t.Fatal("expected silence on underrun")
	}
	// Must not block waiting for producer data — only the pace sleep (~20ms).
	if elapsed > 80*time.Millisecond {
		t.Fatalf("underrun blocked too long: %v", elapsed)
	}
}
