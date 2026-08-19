package wsmedia

import (
	"log/slog"
	"testing"
)

func jitterCfg(jitterMs int) Config {
	return Config{
		Log:            slog.Default(),
		SampleRate:     16000,
		FrameMs:        20,
		JitterBufferMs: jitterMs,
	}
}

func TestConfigJitterNegativeDisables(t *testing.T) {
	cfg := jitterCfg(-40)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if cfg.JitterBufferMs != 0 {
		t.Fatalf("JitterBufferMs=%d, want 0", cfg.JitterBufferMs)
	}
	if got := cfg.JitterPlayoutBytes(); got != 0 {
		t.Fatalf("JitterPlayoutBytes=%d, want 0", got)
	}
}

func TestConfigJitterPlayoutBytes(t *testing.T) {
	cases := []struct {
		jitterMs  int
		wantBytes int // 20ms @ 16kHz PCM16 = 640 bytes/frame
	}{
		{0, 0},
		{10, 640},  // below one frame rounds up
		{20, 640},  // exactly one frame
		{40, 1280}, // two frames
		{50, 1280}, // partial frames truncate
	}
	for _, tc := range cases {
		cfg := jitterCfg(tc.jitterMs)
		if err := cfg.Validate(); err != nil {
			t.Fatalf("validate %dms: %v", tc.jitterMs, err)
		}
		if got := cfg.JitterPlayoutBytes(); got != tc.wantBytes {
			t.Errorf("%dms: JitterPlayoutBytes=%d, want %d", tc.jitterMs, got, tc.wantBytes)
		}
	}
}

func TestConfigJitterKeepsCapacityAboveLead(t *testing.T) {
	cfg := jitterCfg(200)
	cfg.IngressBufferMs = 100
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if cfg.IngressBufferMs <= cfg.JitterBufferMs {
		t.Fatalf("IngressBufferMs=%d not raised above JitterBufferMs=%d",
			cfg.IngressBufferMs, cfg.JitterBufferMs)
	}
	if cfg.IngressBufferBytes() <= cfg.JitterPlayoutBytes() {
		t.Fatalf("capacity %d does not exceed playout lead %d — warm-up unreachable",
			cfg.IngressBufferBytes(), cfg.JitterPlayoutBytes())
	}
}

func TestConfigJitterDefaultCapacityUnchanged(t *testing.T) {
	cfg := jitterCfg(40)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if cfg.IngressBufferMs != DefaultIngressBufferMs {
		t.Fatalf("IngressBufferMs=%d, want the %d default left alone",
			cfg.IngressBufferMs, DefaultIngressBufferMs)
	}
}
