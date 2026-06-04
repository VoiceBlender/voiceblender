package config

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

type Config struct {
	InstanceID            string
	SIPBindIP             string
	SIPBindIPV6           string // IPv6 advertised address; mirrors SIPBindIP for v6 deployments
	SIPListenIP           string
	SIPListenIPV6         string // IPv6 socket bind; falls back to SIPBindIPV6
	SIPExternalIP         string
	SIPPort               string
	SIPTLSPort            string // "" = TLS disabled
	SIPTLSCert            string // path to CA-signed cert (fullchain.pem)
	SIPTLSKey             string // path to private key (privkey.pem)
	SIPDebug              bool   // dump full SIP message content for every request and response
	SIPDomain             string // FQDN advertised in From/Contact/Via for all outbound SIP. Falls back to SIP_EXTERNAL_IP / SIP_BIND_IP when empty.
	SIPHost               string
	HTTPAddr              string
	ICEServers            []string
	RecordingDir          string
	LogLevel              string
	WebhookURL            string
	WebhookSecret         string
	ElevenLabsAPIKey      string
	VAPIAPIKey            string
	DeepgramAPIKey        string
	AzureSpeechKey        string
	AzureSpeechRegion     string
	S3Bucket              string
	S3Region              string
	S3Endpoint            string
	S3Prefix              string
	TTSCacheEnabled       bool
	TTSCacheDir           string
	TTSCacheIncludeAPIKey bool
	RTPPortMin            int
	RTPPortMax            int
	SIPJitterBufferMs     int
	SIPJitterBufferMaxMs  int
	SIPReferAutoDial      bool
	SIPAutoRinging        bool
	SIPUseSourceSocket    bool // when true, send SIP responses and in-dialog requests to the request's source socket instead of Contact / Via sent-by; needed when peers advertise unroutable addresses (e.g. behind NAT)

	SIPRegistrationDefaultExpiresSeconds int
	SIPRegistrationMaxExpiresSeconds     int
	SIPRegistrationSweepIntervalMs       int
	SIPRegistrationAllowMultipleContacts bool
	VSIEventBufferSize                   int
	DefaultSampleRate                    int
	SpeechDetectionEnabled               bool

	MoQEnabled     bool
	MoQListenAddr  string
	MoQTLSCertFile string
	MoQTLSKeyFile  string
	MoQOpusBitrate int

	LiveKitEnabled             bool
	LiveKitURL                 string // wss:// endpoint of the LiveKit server
	LiveKitOpusBitrate         int
	LiveKitTokenSigningEnabled bool   // opt-in: when true, VB can mint JWTs from API key/secret
	LiveKitAPIKey              string // required when LiveKitTokenSigningEnabled=true
	LiveKitAPISecret           string // required when LiveKitTokenSigningEnabled=true; redact in logs
	LiveKitDefaultTokenTTL     time.Duration
}

func Load() Config {
	defaultRate := envInt("DEFAULT_SAMPLE_RATE", 16000)
	if defaultRate != 8000 && defaultRate != 16000 && defaultRate != 48000 {
		defaultRate = 16000
	}
	return Config{
		InstanceID:            envOr("INSTANCE_ID", uuid.New().String()),
		SIPBindIP:             envOr("SIP_BIND_IP", "127.0.0.1"),
		SIPBindIPV6:           os.Getenv("SIP_BIND_IPV6"),   // empty = no IPv6 advertised
		SIPListenIP:           os.Getenv("SIP_LISTEN_IP"),   // empty = same as SIP_BIND_IP
		SIPListenIPV6:         os.Getenv("SIP_LISTEN_IPV6"), // empty = same as SIP_BIND_IPV6
		SIPExternalIP:         os.Getenv("SIP_EXTERNAL_IP"), // public IP for NAT/Docker
		SIPPort:               envOr("SIP_PORT", "5060"),
		SIPTLSPort:            os.Getenv("SIP_TLS_PORT"),
		SIPTLSCert:            os.Getenv("SIP_TLS_CERT"),
		SIPTLSKey:             os.Getenv("SIP_TLS_KEY"),
		SIPDebug:              os.Getenv("SIP_DEBUG") == "true",
		SIPDomain:             os.Getenv("SIP_DOMAIN"),
		SIPHost:               envOr("SIP_HOST", "voiceblender"),
		HTTPAddr:              envOr("HTTP_ADDR", ":8080"),
		ICEServers:            strings.Split(envOr("ICE_SERVERS", "stun:stun.l.google.com:19302"), ","),
		RecordingDir:          envOr("RECORDING_DIR", "/tmp/recordings"),
		LogLevel:              envOr("LOG_LEVEL", "info"),
		WebhookURL:            os.Getenv("WEBHOOK_URL"),
		WebhookSecret:         os.Getenv("WEBHOOK_SECRET"),
		ElevenLabsAPIKey:      os.Getenv("ELEVENLABS_API_KEY"),
		VAPIAPIKey:            os.Getenv("VAPI_API_KEY"),
		DeepgramAPIKey:        os.Getenv("DEEPGRAM_API_KEY"),
		AzureSpeechKey:        os.Getenv("AZURE_SPEECH_KEY"),
		AzureSpeechRegion:     envOr("AZURE_SPEECH_REGION", "eastus"),
		S3Bucket:              os.Getenv("S3_BUCKET"),
		S3Region:              envOr("S3_REGION", "us-east-1"),
		S3Endpoint:            os.Getenv("S3_ENDPOINT"),
		S3Prefix:              os.Getenv("S3_PREFIX"),
		TTSCacheEnabled:       os.Getenv("TTS_CACHE_ENABLED") == "true",
		TTSCacheDir:           envOr("TTS_CACHE_DIR", "/tmp/tts_cache"),
		TTSCacheIncludeAPIKey: os.Getenv("TTS_CACHE_INCLUDE_API_KEY") == "true",
		RTPPortMin:            envInt("RTP_PORT_MIN", 10000),
		RTPPortMax:            envInt("RTP_PORT_MAX", 20000),
		SIPJitterBufferMs:     envInt("SIP_JITTER_BUFFER_MS", 0),
		SIPJitterBufferMaxMs:  envInt("SIP_JITTER_BUFFER_MAX_MS", 300),
		SIPReferAutoDial:      os.Getenv("SIP_REFER_AUTO_DIAL") == "true",
		SIPAutoRinging:        os.Getenv("SIP_AUTO_RINGING") == "true",
		SIPUseSourceSocket:    os.Getenv("SIP_USE_SOURCE_SOCKET") == "true",

		SIPRegistrationDefaultExpiresSeconds: envInt("SIP_REGISTRATION_DEFAULT_EXPIRES_SECONDS", 3600),
		SIPRegistrationMaxExpiresSeconds:     envInt("SIP_REGISTRATION_MAX_EXPIRES_SECONDS", 7200),
		SIPRegistrationSweepIntervalMs:       envInt("SIP_REGISTRATION_SWEEP_INTERVAL_MS", 1000),
		SIPRegistrationAllowMultipleContacts: envBool("SIP_REGISTRATION_ALLOW_MULTIPLE_CONTACTS", true),
		VSIEventBufferSize:                   vsiBufferSize(envInt("VSI_EVENT_BUFFER_SIZE", 256)),
		DefaultSampleRate:                    defaultRate,
		SpeechDetectionEnabled:               os.Getenv("SPEECH_DETECTION_ENABLED") == "true",

		MoQEnabled:     os.Getenv("MOQ_ENABLED") == "true",
		MoQListenAddr:  envOr("MOQ_LISTEN_ADDR", ":8443"),
		MoQTLSCertFile: os.Getenv("MOQ_TLS_CERT_FILE"),
		MoQTLSKeyFile:  os.Getenv("MOQ_TLS_KEY_FILE"),
		MoQOpusBitrate: envInt("MOQ_OPUS_BITRATE", 24000),

		LiveKitEnabled:             os.Getenv("LIVEKIT_ENABLED") == "true",
		LiveKitURL:                 os.Getenv("LIVEKIT_URL"),
		LiveKitOpusBitrate:         envInt("LIVEKIT_OPUS_BITRATE", 24000),
		LiveKitTokenSigningEnabled: os.Getenv("LIVEKIT_TOKEN_SIGNING_ENABLED") == "true",
		LiveKitAPIKey:              os.Getenv("LIVEKIT_API_KEY"),
		LiveKitAPISecret:           os.Getenv("LIVEKIT_API_SECRET"),
		LiveKitDefaultTokenTTL:     envDuration("LIVEKIT_DEFAULT_TOKEN_TTL", 6*time.Hour),
	}
}

// envDuration parses a duration from the environment (Go duration string
// syntax, e.g. "30m", "6h"). Falls back to def on missing or unparseable.
func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

// vsiBufferSize clamps the VSI per-client event buffer to a sane range.
// Below 16 the channel can't absorb even a small burst (one inbound call
// produces ~10 events). The upper bound exists only to guard against
// pathological config — at 1M slots the per-client memory footprint reaches
// roughly 100 MB at typical event sizes.
func vsiBufferSize(n int) int {
	const (
		minSize = 16
		maxSize = 1_000_000
	)
	if n < minSize {
		return minSize
	}
	if n > maxSize {
		return maxSize
	}
	return n
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	switch strings.ToLower(os.Getenv(key)) {
	case "true", "1", "yes", "on":
		return true
	case "false", "0", "no", "off":
		return false
	}
	return def
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
