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

func TestEngine_ExternalIPV6(t *testing.T) {
	engine, err := NewEngine(EngineConfig{
		BindIP:       "127.0.0.1",
		BindIPV6:     "::1",
		ExternalIPV6: "2001:db8::1",
		BindPort:     15062,
		SIPHost:      "test",
		Codecs:       []codec.CodecType{codec.CodecPCMU},
		Log:          slog.Default(),
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	if got := engine.BindIPV6(); got != "2001:db8::1" {
		t.Errorf("BindIPV6() = %q, want 2001:db8::1", got)
	}

	sdp := GenerateOffer(SDPConfig{
		LocalIP: engine.BindIPV6(),
		RTPPort: 10000,
		Codecs:  []codec.CodecType{codec.CodecPCMU},
	})
	if !strings.Contains(string(sdp), "c=IN IP6 2001:db8::1") {
		t.Errorf("SDP missing v6 external IP in c= line:\n%s", sdp)
	}
}

func TestEngine_DualStack(t *testing.T) {
	engine, err := NewEngine(EngineConfig{
		BindIP:       "127.0.0.1",
		BindIPV6:     "::1",
		ExternalIP:   "203.0.113.50",
		ExternalIPV6: "2001:db8::1",
		BindPort:     15063,
		SIPHost:      "test",
		Codecs:       []codec.CodecType{codec.CodecPCMU},
		Log:          slog.Default(),
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	if got := engine.BindIP(); got != "203.0.113.50" {
		t.Errorf("BindIP() = %q, want 203.0.113.50", got)
	}
	if got := engine.BindIPV6(); got != "2001:db8::1" {
		t.Errorf("BindIPV6() = %q, want 2001:db8::1", got)
	}

	if got := engine.AdvertisedIPForFamily("IP4"); got != "203.0.113.50" {
		t.Errorf("AdvertisedIPForFamily(IP4) = %q, want 203.0.113.50", got)
	}
	if got := engine.AdvertisedIPForFamily("IP6"); got != "2001:db8::1" {
		t.Errorf("AdvertisedIPForFamily(IP6) = %q, want 2001:db8::1", got)
	}
}

func TestEngine_AdvertisedIPForFamily_Fallback(t *testing.T) {
	v4Only, err := NewEngine(EngineConfig{
		BindIP: "192.168.1.100", BindPort: 15064, SIPHost: "test",
		Codecs: []codec.CodecType{codec.CodecPCMU}, Log: slog.Default(),
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	if got := v4Only.AdvertisedIPForFamily("IP6"); got != "192.168.1.100" {
		t.Errorf("v4-only fallback for IP6 request = %q, want 192.168.1.100", got)
	}
}

func TestEngine_LegacyExternalIPv6Literal(t *testing.T) {
	engine, err := NewEngine(EngineConfig{
		BindIP:     "127.0.0.1",
		BindIPV6:   "::1",
		ExternalIP: "2001:db8::42",
		BindPort:   15065,
		SIPHost:    "test",
		Codecs:     []codec.CodecType{codec.CodecPCMU},
		Log:        slog.Default(),
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	if got := engine.BindIPV6(); got != "2001:db8::42" {
		t.Errorf("BindIPV6() = %q, want 2001:db8::42 (from legacy SIP_EXTERNAL_IP)", got)
	}
	if got := engine.BindIP(); got != "127.0.0.1" {
		t.Errorf("BindIP() = %q, want 127.0.0.1 (legacy v6 ExternalIP must not stomp v4 advertised)", got)
	}
}
