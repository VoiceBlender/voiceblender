package wsmedia

import (
	"bytes"
	"io"
	"log/slog"
	"testing"
)

func TestStreamBufferPlayoutClampedBelowCap(t *testing.T) {
	const (
		capBytes    = 640
		playoutWant = 639 // clamped from == cap
		frameBytes  = 320
	)
	sb := newStreamBufferPlayout(capBytes, 20, capBytes, true)
	if sb.playoutBytes != playoutWant {
		t.Fatalf("playoutBytes=%d, want %d", sb.playoutBytes, playoutWant)
	}

	// Exact fill to clamped playout (chunked writes that overshoot would drop).
	if _, err := sb.Write(bytes.Repeat([]byte{0xAB}, playoutWant)); err != nil {
		t.Fatalf("write: %v", err)
	}
	out := make([]byte, frameBytes)
	n, err := sb.Read(out)
	if err != nil {
		t.Fatalf("warm read: %v", err)
	}
	if n != frameBytes {
		t.Fatalf("n=%d, want %d", n, frameBytes)
	}
}

func TestStreamBufferCloseShortRead(t *testing.T) {
	sb := newStreamBufferPlayout(1024, 20, 0, true)
	partial := bytes.Repeat([]byte{0x7E}, 50)
	if _, err := sb.Write(partial); err != nil {
		t.Fatalf("write: %v", err)
	}
	sb.Close()
	out := make([]byte, 320)
	n, err := sb.Read(out)
	if err != nil {
		t.Fatalf("short read err=%v", err)
	}
	if n != 50 {
		t.Fatalf("n=%d, want 50", n)
	}
	if !bytes.Equal(out[:n], partial) {
		t.Fatal("short-read payload mismatch")
	}
	n, err = sb.Read(out)
	if n != 0 || err != io.EOF {
		t.Fatalf("second read: n=%d err=%v", n, err)
	}
}

func TestConfigJitterMaxZeroWhenDisabled(t *testing.T) {
	cfg := Config{
		Log:               slog.Default(),
		JitterBufferMs:    0,
		JitterBufferMaxMs: 300,
		IngressBufferMs:   100,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.JitterBufferMaxMs != 0 {
		t.Fatalf("JitterBufferMaxMs=%d, want 0", cfg.JitterBufferMaxMs)
	}
	want := (100 / cfg.FrameMs) * cfg.FrameBytesPCM()
	if got := cfg.IngressBufferBytes(); got != want {
		t.Fatalf("IngressBufferBytes=%d, want %d", got, want)
	}
}

func TestConfigJitterEnsuresCapAbovePlayout(t *testing.T) {
	cfg := Config{
		Log:               slog.Default(),
		SampleRate:        16000,
		FrameMs:           20,
		IngressBufferMs:   40,
		JitterBufferMs:    40,
		JitterBufferMaxMs: 40,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	capB := cfg.IngressBufferBytes()
	playB := cfg.JitterPlayoutBytes()
	if playB >= capB {
		t.Fatalf("playout %d >= cap %d after Validate", playB, capB)
	}
}
