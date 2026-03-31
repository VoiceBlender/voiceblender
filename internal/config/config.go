package config

import (
	"os"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

type Config struct {
	InstanceID            string
	SIPBindIP             string
	SIPListenIP           string
	SIPPort               string
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
}

func Load() Config {
	return Config{
		InstanceID:            envOr("INSTANCE_ID", uuid.New().String()),
		SIPBindIP:             envOr("SIP_BIND_IP", "127.0.0.1"),
		SIPListenIP:           os.Getenv("SIP_LISTEN_IP"), // empty = same as SIP_BIND_IP
		SIPPort:               envOr("SIP_PORT", "5060"),
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
