package sip

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	"github.com/emiago/sipgo/sip"
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

func TestEngine_BindIPV6(t *testing.T) {
	engine, err := NewEngine(EngineConfig{
		BindIP:   "127.0.0.1",
		BindIPV6: "2001:db8::1",
		BindPort: 15062,
		SIPHost:  "test",
		Codecs:   []codec.CodecType{codec.CodecPCMU},
		Log:      slog.Default(),
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
		t.Errorf("SDP missing v6 advertised IP in c= line:\n%s", sdp)
	}
}

func TestEngine_DualStack(t *testing.T) {
	engine, err := NewEngine(EngineConfig{
		BindIP:     "127.0.0.1",
		BindIPV6:   "2001:db8::1",
		ExternalIP: "203.0.113.50",
		BindPort:   15063,
		SIPHost:    "test",
		Codecs:     []codec.CodecType{codec.CodecPCMU},
		Log:        slog.Default(),
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

func TestEngine_PinDestinationToSource(t *testing.T) {
	const (
		source    = "203.0.113.7:5060" // actual UDP source (e.g. behind NAT)
		viaTarget = "192.0.2.1:5060"   // raddr resolved from Via — unroutable
	)

	makeReq := func() *sip.Request {
		req := sip.NewRequest(sip.OPTIONS, sip.Uri{Scheme: "sip", User: "a", Host: "example.com"})
		req.SetSource(source)
		return req
	}

	cases := []struct {
		name     string
		flag     bool
		reqSrc   string
		preset   string // simulated raddr
		expected string
	}{
		{name: "flag on, source available, preset overridden", flag: true, reqSrc: source, preset: viaTarget, expected: source},
		{name: "flag off, preset preserved", flag: false, reqSrc: source, preset: viaTarget, expected: viaTarget},
		{name: "flag on, no source, preset preserved", flag: true, reqSrc: "", preset: viaTarget, expected: viaTarget},
		{name: "flag on, synthetic :port source, preset preserved", flag: true, reqSrc: ":5060", preset: viaTarget, expected: viaTarget},
		{name: "flag on, synthetic :0 source, preset preserved", flag: true, reqSrc: ":0", preset: viaTarget, expected: viaTarget},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := &Engine{useSourceSocket: tc.flag, log: slog.Default()}
			req := makeReq()
			req.SetSource(tc.reqSrc)
			res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
			res.SetDestination(tc.preset)

			e.pinDestinationToSource(req, res)

			if got := res.Destination(); got != tc.expected {
				t.Errorf("Destination = %q, want %q (flag=%v, src=%q)", got, tc.expected, tc.flag, tc.reqSrc)
			}
		})
	}
}

func TestEngine_UseSourceSocketPropagated(t *testing.T) {
	for _, want := range []bool{true, false} {
		want := want
		t.Run("", func(t *testing.T) {
			e, err := NewEngine(EngineConfig{
				BindIP:          "127.0.0.1",
				BindPort:        0,
				SIPHost:         "test",
				Codecs:          []codec.CodecType{codec.CodecPCMU},
				Log:             slog.Default(),
				UseSourceSocket: want,
			})
			if err != nil {
				t.Fatalf("NewEngine: %v", err)
			}
			if got := e.useSourceSocket; got != want {
				t.Errorf("engine.useSourceSocket = %v, want %v", got, want)
			}
		})
	}
}
