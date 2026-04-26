package leg

import (
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	"github.com/pion/webrtc/v4"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func TestPCMedia_PCMUConstruction(t *testing.T) {
	m, err := NewPCMedia(PCMediaConfig{Codec: codec.CodecPCMU, Log: testLogger()})
	if err != nil {
		t.Fatalf("NewPCMedia: %v", err)
	}
	defer m.Close()

	if m.Codec() != codec.CodecPCMU {
		t.Errorf("Codec = %v, want PCMU", m.Codec())
	}
	if m.SampleRate() != 8000 {
		t.Errorf("SampleRate = %d, want 8000", m.SampleRate())
	}
	if m.PC() == nil {
		t.Fatal("PC() returned nil")
	}
	if m.AudioReader() == nil || m.AudioWriter() == nil {
		t.Fatal("AudioReader/AudioWriter returned nil")
	}
}

func TestPCMedia_OpusConstruction(t *testing.T) {
	m, err := NewPCMedia(PCMediaConfig{Codec: codec.CodecOpus, Log: testLogger()})
	if err != nil {
		t.Fatalf("NewPCMedia: %v", err)
	}
	defer m.Close()

	if m.SampleRate() != 48000 {
		t.Errorf("Opus SampleRate = %d, want 48000", m.SampleRate())
	}
}

func TestPCMedia_UnsupportedCodec(t *testing.T) {
	_, err := NewPCMedia(PCMediaConfig{Codec: codec.CodecUnknown, Log: testLogger()})
	if err == nil {
		t.Fatal("expected error for unknown codec")
	}
}

func TestPCMedia_StartIsIdempotent(t *testing.T) {
	m, err := NewPCMedia(PCMediaConfig{Codec: codec.CodecPCMU, Log: testLogger()})
	if err != nil {
		t.Fatalf("NewPCMedia: %v", err)
	}
	defer m.Close()

	m.Start()
	m.Start() // second call must not panic or spawn another writeLoop
}

func TestPCMedia_DrainLocalCandidatesEmpty(t *testing.T) {
	m, err := NewPCMedia(PCMediaConfig{Codec: codec.CodecPCMU, Log: testLogger()})
	if err != nil {
		t.Fatalf("NewPCMedia: %v", err)
	}
	defer m.Close()

	cs, done := m.DrainLocalCandidates()
	if len(cs) != 0 {
		t.Errorf("expected no candidates initially, got %d", len(cs))
	}
	if done {
		t.Error("expected gathering not done initially")
	}
}

// TestPCMedia_Loopback wires two PCMedia objects together over a real
// pion offer/answer exchange and verifies PCM written to one side is
// decoded and readable on the other.
func TestPCMedia_Loopback(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback test involves real ICE/DTLS; skipped in -short")
	}

	caller, err := NewPCMedia(PCMediaConfig{Codec: codec.CodecPCMU, Log: testLogger()})
	if err != nil {
		t.Fatalf("caller NewPCMedia: %v", err)
	}
	defer caller.Close()

	callee, err := NewPCMedia(PCMediaConfig{Codec: codec.CodecPCMU, Log: testLogger()})
	if err != nil {
		t.Fatalf("callee NewPCMedia: %v", err)
	}
	defer callee.Close()

	// Wire trickle ICE end-to-end.
	caller.PC().OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			_ = callee.PC().AddICECandidate(c.ToJSON())
		}
	})
	callee.PC().OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			_ = caller.PC().AddICECandidate(c.ToJSON())
		}
	})

	offer, err := caller.PC().CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	if err := caller.PC().SetLocalDescription(offer); err != nil {
		t.Fatalf("caller SetLocalDescription: %v", err)
	}
	if err := callee.PC().SetRemoteDescription(offer); err != nil {
		t.Fatalf("callee SetRemoteDescription: %v", err)
	}
	answer, err := callee.PC().CreateAnswer(nil)
	if err != nil {
		t.Fatalf("CreateAnswer: %v", err)
	}
	if err := callee.PC().SetLocalDescription(answer); err != nil {
		t.Fatalf("callee SetLocalDescription: %v", err)
	}
	if err := caller.PC().SetRemoteDescription(answer); err != nil {
		t.Fatalf("caller SetRemoteDescription: %v", err)
	}

	caller.Start()
	callee.Start()

	// Wait for ICE to connect.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if caller.PC().ICEConnectionState() == webrtc.ICEConnectionStateConnected &&
			callee.PC().ICEConnectionState() == webrtc.ICEConnectionStateConnected {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if caller.PC().ICEConnectionState() != webrtc.ICEConnectionStateConnected {
		t.Fatalf("caller ICE state = %s", caller.PC().ICEConnectionState())
	}

	// Write one PCM frame on the caller (320 bytes = 160 samples @ 8kHz).
	pcm := make([]byte, 320)
	for i := range pcm {
		pcm[i] = byte(i)
	}

	// Drive writes continuously — ICE/DTLS can drop the first few frames.
	writeCtx := caller.Context()
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(20 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-writeCtx.Done():
				return
			case <-t.C:
				_, _ = caller.AudioWriter().Write(pcm)
			}
		}
	}()

	reader := callee.AudioReader()
	buf := make([]byte, 4096)
	readDeadline := time.Now().Add(15 * time.Second)
	got := 0
	for time.Now().Before(readDeadline) && got == 0 {
		readDone := make(chan int, 1)
		go func() {
			n, _ := io.ReadAtLeast(reader, buf, 1)
			readDone <- n
		}()
		select {
		case n := <-readDone:
			got = n
		case <-time.After(500 * time.Millisecond):
		}
	}
	caller.Close()
	<-done

	if got == 0 {
		t.Fatalf("callee never received decoded PCM within deadline")
	}
}
