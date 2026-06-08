package lkmedia

import (
	"log/slog"
	"strings"
	"testing"
)

func TestConfig_ValidateDefaults(t *testing.T) {
	c := Config{Log: slog.Default()}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if c.SampleRate != DefaultSampleRate {
		t.Errorf("SampleRate = %d, want %d", c.SampleRate, DefaultSampleRate)
	}
	if c.FrameMs != DefaultFrameMs {
		t.Errorf("FrameMs = %d, want %d", c.FrameMs, DefaultFrameMs)
	}
	if c.OpusBitrate != DefaultOpusBitrate {
		t.Errorf("OpusBitrate = %d, want %d", c.OpusBitrate, DefaultOpusBitrate)
	}
	if c.IngressBufferMs != DefaultIngressBufferMs {
		t.Errorf("IngressBufferMs = %d, want %d", c.IngressBufferMs, DefaultIngressBufferMs)
	}
	if c.LaneBufferMs != DefaultLanePCMBufferMs {
		t.Errorf("LaneBufferMs = %d, want %d", c.LaneBufferMs, DefaultLanePCMBufferMs)
	}
}

func TestConfig_RequiresLog(t *testing.T) {
	c := Config{}
	if err := c.Validate(); err == nil {
		t.Fatal("expected error when Log is nil")
	}
}

func TestConfig_RejectsBadSampleRate(t *testing.T) {
	c := Config{Log: slog.Default(), SampleRate: 16000}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "SampleRate") {
		t.Fatalf("expected SampleRate error, got %v", err)
	}
}

func TestConfig_RejectsBadFrameMs(t *testing.T) {
	c := Config{Log: slog.Default(), FrameMs: 7}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "FrameMs") {
		t.Fatalf("expected FrameMs error, got %v", err)
	}
}

func TestConfig_RejectsBadOpusBitrate(t *testing.T) {
	for _, br := range []int{500, 999_999} {
		c := Config{Log: slog.Default(), OpusBitrate: br}
		if err := c.Validate(); err == nil {
			t.Errorf("OpusBitrate=%d: expected error", br)
		}
	}
}

func TestConfig_FrameMath(t *testing.T) {
	c := Config{Log: slog.Default()}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := c.FrameSamples(); got != 960 {
		t.Errorf("FrameSamples = %d, want 960", got)
	}
	if got := c.FrameBytesPCM(); got != 1920 {
		t.Errorf("FrameBytesPCM = %d, want 1920", got)
	}
	if got := c.IngressBufferBytes(); got != 1920*50 {
		t.Errorf("IngressBufferBytes = %d, want %d", got, 1920*50)
	}
	if got := c.LaneCapacityFrames(); got != 10 {
		t.Errorf("LaneCapacityFrames = %d, want 10", got)
	}
}
