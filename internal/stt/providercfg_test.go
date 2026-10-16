package stt

import (
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/providercfg"
)

func TestDeepgramURLs_RegionAndMIPOptOut(t *testing.T) {
	eu := providercfg.Deepgram{BaseURL: "https://api.eu.deepgram.com", MIPOptOut: true}

	if got, want := buildDeepgramV1URL(eu, Options{}),
		"wss://api.eu.deepgram.com/v1/listen?encoding=linear16&sample_rate=16000&channels=1&model=nova-3&language=en&mip_opt_out=true"; got != want {
		t.Errorf("v1 URL\n got %s\nwant %s", got, want)
	}
	if got, want := buildFluxURL(eu, Options{}),
		"wss://api.eu.deepgram.com/v2/listen?encoding=linear16&sample_rate=16000&model=flux-general-en&mip_opt_out=true"; got != want {
		t.Errorf("flux URL\n got %s\nwant %s", got, want)
	}

	us := providercfg.Deepgram{MIPOptOut: true}
	if got, want := buildDeepgramV1URL(us, Options{}),
		"wss://api.deepgram.com/v1/listen?encoding=linear16&sample_rate=16000&channels=1&model=nova-3&language=en&mip_opt_out=true"; got != want {
		t.Errorf("default-region v1 URL\n got %s\nwant %s", got, want)
	}
}

func TestBuildElevenLabsSTTURL(t *testing.T) {
	cases := []struct {
		name string
		cfg  providercfg.ElevenLabs
		opts Options
		want string
	}{
		{
			name: "defaults",
			want: "wss://api.elevenlabs.io/v1/speech-to-text/realtime?model_id=scribe_v2_realtime&language_code=en&audio_format=pcm_16000&commit_strategy=vad",
		},
		{
			name: "eu residency, zero retention",
			cfg:  providercfg.ElevenLabs{BaseURL: "https://api.eu.residency.elevenlabs.io", DisableLogging: true},
			opts: Options{Language: "de"},
			want: "wss://api.eu.residency.elevenlabs.io/v1/speech-to-text/realtime?model_id=scribe_v2_realtime&language_code=de&audio_format=pcm_16000&commit_strategy=vad&enable_logging=false",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildElevenLabsSTTURL(tc.cfg, tc.opts); got != tc.want {
				t.Errorf("\n got %s\nwant %s", got, tc.want)
			}
		})
	}
}
