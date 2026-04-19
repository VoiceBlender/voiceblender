package config

import (
	"testing"
)

func TestLoad_Defaults(t *testing.T) {
	// Clear env vars that could be set externally.
	for _, key := range []string{
		"INSTANCE_ID", "SIP_BIND_IP", "SIP_LISTEN_IP", "SIP_PORT", "SIP_HOST",
		"HTTP_ADDR", "ICE_SERVERS", "RECORDING_DIR", "LOG_LEVEL", "WEBHOOK_URL",
		"WEBHOOK_SECRET", "RTP_PORT_MIN", "RTP_PORT_MAX",
		"TTS_CACHE_ENABLED", "TTS_CACHE_DIR", "TTS_CACHE_INCLUDE_API_KEY",
		"AZURE_SPEECH_KEY", "AZURE_SPEECH_REGION", "DEFAULT_SAMPLE_RATE",
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

	cfg := Load()

	if !cfg.TTSCacheEnabled {
		t.Error("TTSCacheEnabled should be true")
	}
	if !cfg.TTSCacheIncludeAPIKey {
		t.Error("TTSCacheIncludeAPIKey should be true")
	}
}

func TestLoad_BooleanFields_False(t *testing.T) {
	t.Setenv("TTS_CACHE_ENABLED", "false")
	t.Setenv("TTS_CACHE_INCLUDE_API_KEY", "")

	cfg := Load()

	if cfg.TTSCacheEnabled {
		t.Error("TTSCacheEnabled should be false")
	}
	if cfg.TTSCacheIncludeAPIKey {
		t.Error("TTSCacheIncludeAPIKey should be false")
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
