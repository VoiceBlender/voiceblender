package lkmedia

import (
	"math"
	"testing"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
)

func TestAudioLevelFromPCM_Silence(t *testing.T) {
	level, voice := audioLevelFromPCM(make([]int16, 960))
	if level != 127 {
		t.Fatalf("silence: expected level 127, got %d", level)
	}
	if voice {
		t.Fatalf("silence: voice flag should be false")
	}
}

func TestAudioLevelFromPCM_EmptyFrame(t *testing.T) {
	level, voice := audioLevelFromPCM(nil)
	if level != 127 || voice {
		t.Fatalf("empty: expected (127, false), got (%d, %v)", level, voice)
	}
}

func TestAudioLevelFromPCM_FullScale(t *testing.T) {
	samples := make([]int16, 960)
	for i := range samples {
		samples[i] = math.MaxInt16
	}
	level, voice := audioLevelFromPCM(samples)
	if level != 0 {
		t.Fatalf("full-scale: expected level 0, got %d", level)
	}
	if !voice {
		t.Fatalf("full-scale: expected voice=true")
	}
}

func TestAudioLevelFromPCM_QuietSignal(t *testing.T) {
	samples := make([]int16, 960)
	for i := range samples {
		samples[i] = 8 // ~-72 dBov; well below the voice threshold
	}
	level, voice := audioLevelFromPCM(samples)
	if level < 60 || level > 90 {
		t.Fatalf("quiet: expected level around -72 dBov, got %d", level)
	}
	if voice {
		t.Fatalf("quiet: voice flag should be false (level=%d)", level)
	}
}

func TestAudioLevelFromPCM_LoudSignalTriggersVoice(t *testing.T) {
	samples := make([]int16, 960)
	for i := range samples {
		samples[i] = 8000 // ~-12 dBov; clearly speech-volume
	}
	level, voice := audioLevelFromPCM(samples)
	if level > 20 {
		t.Fatalf("loud: expected level <= 20, got %d", level)
	}
	if !voice {
		t.Fatalf("loud: voice flag should be true (level=%d)", level)
	}
}

// fakeLevelProvider returns a constant level/voice pair for the
// interceptor binding test.
type fakeLevelProvider struct {
	level uint8
	voice bool
}

func (f fakeLevelProvider) AudioLevel() (uint8, bool) { return f.level, f.voice }

func TestAudioLevelInterceptor_StampsExtensionWhenNegotiated(t *testing.T) {
	provider := fakeLevelProvider{level: 23, voice: true}
	ic := newAudioLevelInterceptor(provider)

	var captured *rtp.Header
	chain := ic.BindLocalStream(
		&interceptor.StreamInfo{
			RTPHeaderExtensions: []interceptor.RTPHeaderExtension{
				{URI: sdp.AudioLevelURI, ID: 9},
			},
		},
		interceptor.RTPWriterFunc(func(h *rtp.Header, _ []byte, _ interceptor.Attributes) (int, error) {
			captured = h
			return 0, nil
		}),
	)

	if _, err := chain.Write(&rtp.Header{Version: 2}, nil, nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	if captured == nil {
		t.Fatalf("inner writer not invoked")
	}
	raw := captured.GetExtension(9)
	if len(raw) == 0 {
		t.Fatalf("audio-level extension not attached to header")
	}
	var ext rtp.AudioLevelExtension
	if err := ext.Unmarshal(raw); err != nil {
		t.Fatalf("unmarshal ext: %v", err)
	}
	if ext.Level != 23 || !ext.Voice {
		t.Fatalf("expected level=23 voice=true, got level=%d voice=%v", ext.Level, ext.Voice)
	}
}

func TestAudioLevelInterceptor_PassThroughWhenNotNegotiated(t *testing.T) {
	ic := newAudioLevelInterceptor(fakeLevelProvider{level: 50})

	var captured *rtp.Header
	chain := ic.BindLocalStream(
		&interceptor.StreamInfo{}, // no extensions negotiated
		interceptor.RTPWriterFunc(func(h *rtp.Header, _ []byte, _ interceptor.Attributes) (int, error) {
			captured = h
			return 0, nil
		}),
	)
	if _, err := chain.Write(&rtp.Header{Version: 2}, nil, nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	if captured == nil {
		t.Fatalf("inner writer not invoked")
	}
	if len(captured.Extensions) != 0 {
		t.Fatalf("expected no extensions when URI not negotiated, got %d", len(captured.Extensions))
	}
}
