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
