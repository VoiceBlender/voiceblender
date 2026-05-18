package moqmedia

import (
	"log/slog"
	"strings"
	"testing"
)

func newLog() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func TestConfig_Validate_Defaults(t *testing.T) {
	c := Config{Log: newLog()}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if c.SampleRate != 48000 {
		t.Errorf("SampleRate = %d, want 48000", c.SampleRate)
	}
	if c.FrameMs != 20 {
		t.Errorf("FrameMs = %d, want 20", c.FrameMs)
	}
	if c.OpusBitrate != DefaultOpusBitrate {
		t.Errorf("OpusBitrate = %d, want %d", c.OpusBitrate, DefaultOpusBitrate)
	}
	if c.IngressBufferMs != DefaultIngressBufferMs {
		t.Errorf("IngressBufferMs = %d, want %d", c.IngressBufferMs, DefaultIngressBufferMs)
	}
}

func TestConfig_Validate_MissingLog(t *testing.T) {
	c := Config{}
	if err := c.Validate(); err == nil {
		t.Fatal("expected Validate to fail without Log")
	}
}

func TestConfig_Validate_RejectsBadSampleRate(t *testing.T) {
	c := Config{Log: newLog(), SampleRate: 16000}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "SampleRate") {
		t.Fatalf("expected SampleRate error, got %v", err)
	}
}

func TestConfig_Validate_RejectsBadFrameMs(t *testing.T) {
	c := Config{Log: newLog(), FrameMs: 7}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "FrameMs") {
		t.Fatalf("expected FrameMs error, got %v", err)
	}
}

func TestConfig_Validate_RejectsBadBitrate(t *testing.T) {
	c := Config{Log: newLog(), OpusBitrate: 100}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "OpusBitrate") {
		t.Fatalf("expected OpusBitrate error, got %v", err)
	}
}

func TestConfig_FrameMath(t *testing.T) {
	c := Config{Log: newLog()}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := c.FrameSamples(); got != 960 {
		t.Errorf("FrameSamples = %d, want 960 (48k * 20ms)", got)
	}
	if got := c.FrameBytesPCM(); got != 1920 {
		t.Errorf("FrameBytesPCM = %d, want 1920", got)
	}
	if got := c.IngressBufferBytes(); got != 1920*50 {
		t.Errorf("IngressBufferBytes = %d, want %d", got, 1920*50)
	}
}
