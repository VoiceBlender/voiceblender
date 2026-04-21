package sip

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/codec"
)

func TestEngine_ExternalIP(t *testing.T) {
	engine, err := NewEngine(EngineConfig{
		BindIP:     "127.0.0.1",
		ExternalIP: "203.0.113.50",
		BindPort:   15060,
		SIPHost:    "test",
		Codecs:     []codec.CodecType{codec.CodecPCMU},
		Log:        slog.Default(),
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	if engine.BindIP() != "203.0.113.50" {
		t.Errorf("BindIP() = %q, want 203.0.113.50", engine.BindIP())
	}

	// Verify SDP contains the external IP in c= line.
	sdp := GenerateOffer(SDPConfig{
		LocalIP: engine.BindIP(),
		RTPPort: 10000,
		Codecs:  []codec.CodecType{codec.CodecPCMU},
	})
	if !strings.Contains(string(sdp), "c=IN IP4 203.0.113.50") {
		t.Errorf("SDP missing external IP in c= line:\n%s", sdp)
	}
}

func TestEngine_NoExternalIP(t *testing.T) {
	engine, err := NewEngine(EngineConfig{
		BindIP:   "192.168.1.100",
		BindPort: 15061,
		SIPHost:  "test",
		Codecs:   []codec.CodecType{codec.CodecPCMU},
		Log:      slog.Default(),
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	if engine.BindIP() != "192.168.1.100" {
		t.Errorf("BindIP() = %q, want 192.168.1.100", engine.BindIP())
	}
}
