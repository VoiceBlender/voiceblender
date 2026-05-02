package config

import (
	"os"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

type Config struct {
	InstanceID             string
	SIPBindIP              string
	SIPBindIPV6            string // IPv6 advertised address; mirrors SIPBindIP for v6 deployments
	SIPListenIP            string
	SIPListenIPV6          string // IPv6 socket bind; falls back to SIPBindIPV6
	SIPExternalIP          string
	SIPPort                string
	SIPTLSPort             string // "" = TLS disabled
	SIPTLSCert             string // path to CA-signed cert (fullchain.pem)
	SIPTLSKey              string // path to private key (privkey.pem)
	SIPDebug               bool   // dump full SIP message content for every request and response
	SIPDomain              string // FQDN advertised in From/Contact/Via for all outbound SIP. Falls back to SIP_EXTERNAL_IP / SIP_BIND_IP when empty.
	SIPHost                string
	HTTPAddr               string
	ICEServers             []string
	RecordingDir           string
	LogLevel               string
	WebhookURL             string
	WebhookSecret          string
	ElevenLabsAPIKey       string
	VAPIAPIKey             string
	DeepgramAPIKey         string
	AzureSpeechKey         string
	AzureSpeechRegion      string
	S3Bucket               string
	S3Region               string
	S3Endpoint             string
	S3Prefix               string
	TTSCacheEnabled        bool
	TTSCacheDir            string
	TTSCacheIncludeAPIKey  bool
	RTPPortMin             int
	RTPPortMax             int
	SIPJitterBufferMs      int
	SIPJitterBufferMaxMs   int
	SIPReferAutoDial       bool
	SIPAutoRinging         bool
	DefaultSampleRate      int
	SpeechDetectionEnabled bool
}

func Load() Config {
	defaultRate := envInt("DEFAULT_SAMPLE_RATE", 16000)
	if defaultRate != 8000 && defaultRate != 16000 && defaultRate != 48000 {
		defaultRate = 16000
	}
	return Config{
		InstanceID:             envOr("INSTANCE_ID", uuid.New().String()),
		SIPBindIP:              envOr("SIP_BIND_IP", "127.0.0.1"),
		SIPBindIPV6:            os.Getenv("SIP_BIND_IPV6"),   // empty = no IPv6 advertised
		SIPListenIP:            os.Getenv("SIP_LISTEN_IP"),   // empty = same as SIP_BIND_IP
		SIPListenIPV6:          os.Getenv("SIP_LISTEN_IPV6"), // empty = same as SIP_BIND_IPV6
		SIPExternalIP:          os.Getenv("SIP_EXTERNAL_IP"), // public IP for NAT/Docker
		SIPPort:                envOr("SIP_PORT", "5060"),
		SIPTLSPort:             os.Getenv("SIP_TLS_PORT"),
		SIPTLSCert:             os.Getenv("SIP_TLS_CERT"),
		SIPTLSKey:              os.Getenv("SIP_TLS_KEY"),
		SIPDebug:               os.Getenv("SIP_DEBUG") == "true",
		SIPDomain:              os.Getenv("SIP_DOMAIN"),
		SIPHost:                envOr("SIP_HOST", "voiceblender"),
		HTTPAddr:               envOr("HTTP_ADDR", ":8080"),
		ICEServers:             strings.Split(envOr("ICE_SERVERS", "stun:stun.l.google.com:19302"), ","),
		RecordingDir:           envOr("RECORDING_DIR", "/tmp/recordings"),
		LogLevel:               envOr("LOG_LEVEL", "info"),
		WebhookURL:             os.Getenv("WEBHOOK_URL"),
		WebhookSecret:          os.Getenv("WEBHOOK_SECRET"),
		ElevenLabsAPIKey:       os.Getenv("ELEVENLABS_API_KEY"),
		VAPIAPIKey:             os.Getenv("VAPI_API_KEY"),
		DeepgramAPIKey:         os.Getenv("DEEPGRAM_API_KEY"),
		AzureSpeechKey:         os.Getenv("AZURE_SPEECH_KEY"),
		AzureSpeechRegion:      envOr("AZURE_SPEECH_REGION", "eastus"),
		S3Bucket:               os.Getenv("S3_BUCKET"),
		S3Region:               envOr("S3_REGION", "us-east-1"),
		S3Endpoint:             os.Getenv("S3_ENDPOINT"),
		S3Prefix:               os.Getenv("S3_PREFIX"),
		TTSCacheEnabled:        os.Getenv("TTS_CACHE_ENABLED") == "true",
		TTSCacheDir:            envOr("TTS_CACHE_DIR", "/tmp/tts_cache"),
		TTSCacheIncludeAPIKey:  os.Getenv("TTS_CACHE_INCLUDE_API_KEY") == "true",
		RTPPortMin:             envInt("RTP_PORT_MIN", 10000),
		RTPPortMax:             envInt("RTP_PORT_MAX", 20000),
		SIPJitterBufferMs:      envInt("SIP_JITTER_BUFFER_MS", 0),
		SIPJitterBufferMaxMs:   envInt("SIP_JITTER_BUFFER_MAX_MS", 300),
		SIPReferAutoDial:       os.Getenv("SIP_REFER_AUTO_DIAL") == "true",
		SIPAutoRinging:         os.Getenv("SIP_AUTO_RINGING") == "true",
		DefaultSampleRate:      defaultRate,
		SpeechDetectionEnabled: os.Getenv("SPEECH_DETECTION_ENABLED") == "true",
	}
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
