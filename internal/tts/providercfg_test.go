package tts

import (
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/providercfg"
)

func TestBuildDeepgramSpeakURL(t *testing.T) {
	if got, want := buildDeepgramSpeakURL(providercfg.Deepgram{}, "aura-2-asteria-en"),
		"https://api.deepgram.com/v1/speak?model=aura-2-asteria-en&encoding=linear16&sample_rate=16000&container=none"; got != want {
		t.Errorf("default\n got %s\nwant %s", got, want)
	}
	eu := providercfg.Deepgram{BaseURL: "https://api.eu.deepgram.com", MIPOptOut: true}
	if got, want := buildDeepgramSpeakURL(eu, "aura-2-asteria-en"),
		"https://api.eu.deepgram.com/v1/speak?model=aura-2-asteria-en&encoding=linear16&sample_rate=16000&container=none&mip_opt_out=true"; got != want {
		t.Errorf("eu + opt-out\n got %s\nwant %s", got, want)
	}
}

func TestBuildElevenLabsTTSURL(t *testing.T) {
	if got, want := buildElevenLabsTTSURL(providercfg.ElevenLabs{}, "voice123"),
		"https://api.elevenlabs.io/v1/text-to-speech/voice123/stream?output_format=pcm_16000"; got != want {
		t.Errorf("default\n got %s\nwant %s", got, want)
	}
	eu := providercfg.ElevenLabs{BaseURL: "https://api.eu.residency.elevenlabs.io", DisableLogging: true}
	if got, want := buildElevenLabsTTSURL(eu, "voice123"),
		"https://api.eu.residency.elevenlabs.io/v1/text-to-speech/voice123/stream?output_format=pcm_16000&enable_logging=false"; got != want {
		t.Errorf("eu + zero retention\n got %s\nwant %s", got, want)
	}
}
