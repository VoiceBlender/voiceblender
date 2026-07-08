package config

import (
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/codec"
)

func TestLoad_Defaults(t *testing.T) {
	// Clear env vars that could be set externally.
	for _, key := range []string{
		"INSTANCE_ID", "SIP_BIND_IP", "SIP_LISTEN_IP", "SIP_PORT", "SIP_HOST",
		"HTTP_ADDR", "ICE_SERVERS", "RECORDING_DIR", "LOG_LEVEL", "WEBHOOK_URL",
		"WEBHOOK_SECRET", "RTP_PORT_MIN", "RTP_PORT_MAX",
		"TTS_CACHE_ENABLED", "TTS_CACHE_DIR", "TTS_CACHE_INCLUDE_API_KEY",
		"AZURE_SPEECH_KEY", "AZURE_SPEECH_REGION", "DEFAULT_SAMPLE_RATE",
		"MOQ_ENABLED", "MOQ_LISTEN_ADDR", "MOQ_TLS_CERT_FILE", "MOQ_TLS_KEY_FILE",
		"MOQ_OPUS_BITRATE", "AMRWB_MODE", "AMRWB_OCTET_ALIGNED",
		"AMRNB_MODE", "AMRNB_OCTET_ALIGNED", "SIP_CODECS",
		"SIP_INBOUND_REGISTER_DEFAULT",
	} {
		t.Setenv(key, "")
	}

	cfg := Load()

	if cfg.InstanceID == "" {
		t.Fatal("expected auto-generated InstanceID")
	}
	if cfg.SIPBindIP != "127.0.0.1" {
		t.Errorf("SIPBindIP = %q, want 127.0.0.1", cfg.SIPBindIP)
	}
	if cfg.SIPPort != "5060" {
		t.Errorf("SIPPort = %q, want 5060", cfg.SIPPort)
	}
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q, want :8080", cfg.HTTPAddr)
	}
	if cfg.RecordingDir != "/tmp/recordings" {
		t.Errorf("RecordingDir = %q, want /tmp/recordings", cfg.RecordingDir)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want info", cfg.LogLevel)
	}
	if cfg.RTPPortMin != 10000 {
		t.Errorf("RTPPortMin = %d, want 10000", cfg.RTPPortMin)
	}
	if cfg.RTPPortMax != 20000 {
		t.Errorf("RTPPortMax = %d, want 20000", cfg.RTPPortMax)
	}
	if cfg.TTSCacheDir != "/tmp/tts_cache" {
		t.Errorf("TTSCacheDir = %q, want /tmp/tts_cache", cfg.TTSCacheDir)
	}
	if cfg.AzureSpeechRegion != "eastus" {
		t.Errorf("AzureSpeechRegion = %q, want eastus", cfg.AzureSpeechRegion)
	}
	if cfg.AzureSpeechKey != "" {
		t.Errorf("AzureSpeechKey = %q, want empty", cfg.AzureSpeechKey)
	}
	if cfg.DefaultSampleRate != 16000 {
		t.Errorf("DefaultSampleRate = %d, want 16000", cfg.DefaultSampleRate)
	}
	if cfg.SIPInboundRegisterDefault != "reject" {
		t.Errorf("SIPInboundRegisterDefault = %q, want reject (fail-closed default)", cfg.SIPInboundRegisterDefault)
	}
	if cfg.MoQEnabled {
		t.Error("MoQEnabled should default to false")
	}
	if cfg.MoQListenAddr != ":8443" {
		t.Errorf("MoQListenAddr = %q, want :8443", cfg.MoQListenAddr)
	}
	if cfg.MoQTLSCertFile != "" {
		t.Errorf("MoQTLSCertFile = %q, want empty", cfg.MoQTLSCertFile)
	}
	if cfg.MoQTLSKeyFile != "" {
		t.Errorf("MoQTLSKeyFile = %q, want empty", cfg.MoQTLSKeyFile)
	}
	if cfg.MoQOpusBitrate != 24000 {
		t.Errorf("MoQOpusBitrate = %d, want 24000", cfg.MoQOpusBitrate)
	}
	if cfg.AMRWBMode != 2 {
		t.Errorf("AMRWBMode = %d, want 2", cfg.AMRWBMode)
	}
	if !cfg.AMRWBOctetAligned {
		t.Error("AMRWBOctetAligned should default to true")
	}
	wantCodecs := []codec.CodecType{codec.CodecPCMU, codec.CodecPCMA}
	if len(cfg.Codecs) != len(wantCodecs) {
		t.Fatalf("Codecs = %v, want %v", cfg.Codecs, wantCodecs)
	}
	for i, c := range wantCodecs {
		if cfg.Codecs[i] != c {
			t.Errorf("Codecs[%d] = %s, want %s", i, cfg.Codecs[i], c)
		}
	}
}

func TestLoad_SIPCodecs(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want []codec.CodecType
	}{
		{
			name: "single codec",
			env:  "opus",
			want: []codec.CodecType{codec.CodecOpus},
		},
		{
			name: "preference order preserved",
			env:  "opus,G722,PCMU,PCMA",
			want: []codec.CodecType{codec.CodecOpus, codec.CodecG722, codec.CodecPCMU, codec.CodecPCMA},
		},
		{
			name: "whitespace trimmed, unknowns dropped, dupes deduped",
			env:  " PCMU , garbage, PCMU , AMR-NB ",
			want: []codec.CodecType{codec.CodecPCMU, codec.CodecAMRNB},
		},
		{
			name: "AMR aliases resolve",
			env:  "AMR-WB,AMR",
			want: []codec.CodecType{codec.CodecAMRWB, codec.CodecAMRNB},
		},
		{
			name: "all unknown falls back to default (PCMU,PCMA)",
			env:  "foo,bar",
			want: []codec.CodecType{codec.CodecPCMU, codec.CodecPCMA},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SIP_CODECS", tc.env)
			got := Load().Codecs
			if len(got) != len(tc.want) {
				t.Fatalf("Codecs = %v, want %v", got, tc.want)
			}
			for i, c := range tc.want {
				if got[i] != c {
					t.Errorf("Codecs[%d] = %s, want %s", i, got[i], c)
				}
			}
		})
	}
}

func TestLoad_AMRWB(t *testing.T) {
	t.Setenv("AMRWB_MODE", "2")
	t.Setenv("AMRWB_OCTET_ALIGNED", "false")
	cfg := Load()
	if cfg.AMRWBMode != 2 {
		t.Errorf("AMRWBMode = %d, want 2", cfg.AMRWBMode)
	}
	if cfg.AMRWBOctetAligned {
		t.Error("AMRWBOctetAligned = true, want false")
	}

	// Out-of-range modes clamp to the valid 0..8 range.
	t.Setenv("AMRWB_MODE", "42")
	if got := Load().AMRWBMode; got != 8 {
		t.Errorf("AMRWBMode(42) = %d, want clamped 8", got)
	}
	t.Setenv("AMRWB_MODE", "-3")
	if got := Load().AMRWBMode; got != 0 {
		t.Errorf("AMRWBMode(-3) = %d, want clamped 0", got)
	}
}

func TestLoad_EnvOverrides(t *testing.T) {
	t.Setenv("INSTANCE_ID", "test-123")
	t.Setenv("SIP_BIND_IP", "10.0.0.1")
	t.Setenv("SIP_LISTEN_IP", "0.0.0.0")
	t.Setenv("SIP_PORT", "5080")
	t.Setenv("SIP_HOST", "sip.example.com")
	t.Setenv("HTTP_ADDR", ":9090")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("WEBHOOK_URL", "https://example.com/hook")
	t.Setenv("WEBHOOK_SECRET", "s3cret")
	t.Setenv("RTP_PORT_MIN", "30000")
	t.Setenv("RTP_PORT_MAX", "40000")
	t.Setenv("AZURE_SPEECH_KEY", "az-key-123")
	t.Setenv("AZURE_SPEECH_REGION", "westeurope")
	t.Setenv("DEFAULT_SAMPLE_RATE", "48000")

	cfg := Load()

	if cfg.InstanceID != "test-123" {
		t.Errorf("InstanceID = %q, want test-123", cfg.InstanceID)
	}
	if cfg.SIPBindIP != "10.0.0.1" {
		t.Errorf("SIPBindIP = %q, want 10.0.0.1", cfg.SIPBindIP)
	}
	if cfg.SIPListenIP != "0.0.0.0" {
		t.Errorf("SIPListenIP = %q, want 0.0.0.0", cfg.SIPListenIP)
	}
	if cfg.SIPPort != "5080" {
		t.Errorf("SIPPort = %q, want 5080", cfg.SIPPort)
	}
	if cfg.SIPHost != "sip.example.com" {
		t.Errorf("SIPHost = %q, want sip.example.com", cfg.SIPHost)
	}
	if cfg.HTTPAddr != ":9090" {
		t.Errorf("HTTPAddr = %q, want :9090", cfg.HTTPAddr)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", cfg.LogLevel)
	}
	if cfg.WebhookURL != "https://example.com/hook" {
		t.Errorf("WebhookURL = %q, want https://example.com/hook", cfg.WebhookURL)
	}
	if cfg.WebhookSecret != "s3cret" {
		t.Errorf("WebhookSecret = %q, want s3cret", cfg.WebhookSecret)
	}
	if cfg.RTPPortMin != 30000 {
		t.Errorf("RTPPortMin = %d, want 30000", cfg.RTPPortMin)
	}
	if cfg.RTPPortMax != 40000 {
		t.Errorf("RTPPortMax = %d, want 40000", cfg.RTPPortMax)
	}
	if cfg.AzureSpeechKey != "az-key-123" {
		t.Errorf("AzureSpeechKey = %q, want az-key-123", cfg.AzureSpeechKey)
	}
	if cfg.AzureSpeechRegion != "westeurope" {
		t.Errorf("AzureSpeechRegion = %q, want westeurope", cfg.AzureSpeechRegion)
	}
	if cfg.DefaultSampleRate != 48000 {
		t.Errorf("DefaultSampleRate = %d, want 48000", cfg.DefaultSampleRate)
	}
}

func TestLoad_MoQOverrides(t *testing.T) {
	t.Setenv("MOQ_ENABLED", "true")
	t.Setenv("MOQ_LISTEN_ADDR", ":9443")
	t.Setenv("MOQ_TLS_CERT_FILE", "/etc/voiceblender/cert.pem")
	t.Setenv("MOQ_TLS_KEY_FILE", "/etc/voiceblender/key.pem")
	t.Setenv("MOQ_OPUS_BITRATE", "32000")

	cfg := Load()

	if !cfg.MoQEnabled {
		t.Error("MoQEnabled should be true")
	}
	if cfg.MoQListenAddr != ":9443" {
		t.Errorf("MoQListenAddr = %q, want :9443", cfg.MoQListenAddr)
	}
	if cfg.MoQTLSCertFile != "/etc/voiceblender/cert.pem" {
		t.Errorf("MoQTLSCertFile = %q, want /etc/voiceblender/cert.pem", cfg.MoQTLSCertFile)
	}
	if cfg.MoQTLSKeyFile != "/etc/voiceblender/key.pem" {
		t.Errorf("MoQTLSKeyFile = %q, want /etc/voiceblender/key.pem", cfg.MoQTLSKeyFile)
	}
	if cfg.MoQOpusBitrate != 32000 {
		t.Errorf("MoQOpusBitrate = %d, want 32000", cfg.MoQOpusBitrate)
	}
}

func TestLoad_DefaultSampleRate_Invalid(t *testing.T) {
	t.Setenv("DEFAULT_SAMPLE_RATE", "44100")
	cfg := Load()
	if cfg.DefaultSampleRate != 16000 {
		t.Errorf("DefaultSampleRate = %d, want 16000 (fallback for invalid value)", cfg.DefaultSampleRate)
	}
}

func TestLoad_BooleanFields(t *testing.T) {
	t.Setenv("TTS_CACHE_ENABLED", "true")
	t.Setenv("TTS_CACHE_INCLUDE_API_KEY", "true")
	t.Setenv("SIP_USE_SOURCE_SOCKET", "true")

	cfg := Load()

	if !cfg.TTSCacheEnabled {
		t.Error("TTSCacheEnabled should be true")
	}
	if !cfg.TTSCacheIncludeAPIKey {
		t.Error("TTSCacheIncludeAPIKey should be true")
	}
	if !cfg.SIPUseSourceSocket {
		t.Error("SIPUseSourceSocket should be true")
	}
}

func TestLoad_BooleanFields_False(t *testing.T) {
	t.Setenv("TTS_CACHE_ENABLED", "false")
	t.Setenv("TTS_CACHE_INCLUDE_API_KEY", "")
	t.Setenv("SIP_USE_SOURCE_SOCKET", "")

	cfg := Load()

	if cfg.TTSCacheEnabled {
		t.Error("TTSCacheEnabled should be false")
	}
	if cfg.TTSCacheIncludeAPIKey {
		t.Error("TTSCacheIncludeAPIKey should be false")
	}
	if cfg.SIPUseSourceSocket {
		t.Error("SIPUseSourceSocket should be false")
	}
}

func TestLoad_ICEServers(t *testing.T) {
	t.Setenv("ICE_SERVERS", "stun:stun1.example.com,stun:stun2.example.com")

	cfg := Load()

	if len(cfg.ICEServers) != 2 {
		t.Fatalf("ICEServers len = %d, want 2", len(cfg.ICEServers))
	}
	if cfg.ICEServers[0] != "stun:stun1.example.com" {
		t.Errorf("ICEServers[0] = %q, want stun:stun1.example.com", cfg.ICEServers[0])
	}
	if cfg.ICEServers[1] != "stun:stun2.example.com" {
		t.Errorf("ICEServers[1] = %q, want stun:stun2.example.com", cfg.ICEServers[1])
	}
}

func TestLoad_ICEServers_Empty(t *testing.T) {
	t.Setenv("ICE_SERVERS", "")

	cfg := Load()

	// When ICE_SERVERS is empty, the default STUN server is used.
	if len(cfg.ICEServers) != 1 || cfg.ICEServers[0] != "stun:stun.l.google.com:19302" {
		t.Errorf("ICEServers = %v, want [stun:stun.l.google.com:19302]", cfg.ICEServers)
	}
}

func TestEnvInt_Valid(t *testing.T) {
	if got := envInt("NONEXISTENT_TEST_VAR_12345", 42); got != 42 {
		t.Errorf("envInt default = %d, want 42", got)
	}
}

func TestEnvInt_InvalidFallsBack(t *testing.T) {
	t.Setenv("TEST_ENV_INT", "notanumber")
	if got := envInt("TEST_ENV_INT", 99); got != 99 {
		t.Errorf("envInt invalid = %d, want 99", got)
	}
}

func TestEnvOr_Default(t *testing.T) {
	if got := envOr("NONEXISTENT_TEST_VAR_12345", "default"); got != "default" {
		t.Errorf("envOr default = %q, want default", got)
	}
}

func TestEnvOr_Override(t *testing.T) {
	t.Setenv("TEST_ENV_OR", "override")
	if got := envOr("TEST_ENV_OR", "default"); got != "override" {
		t.Errorf("envOr = %q, want override", got)
	}
}
