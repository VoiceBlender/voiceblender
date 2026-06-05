package lkmedia

import (
	"math"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
)

// audioLevelProvider supplies the current RFC 6464 audio level for the
// most recently encoded outgoing Opus frame. Returned level uses the
// standard inverted scale: 0 = digital full-scale, 127 = digital silence.
type audioLevelProvider interface {
	AudioLevel() (level uint8, voice bool)
}

// audioLevelInterceptor stamps every outgoing audio RTP packet with the
// `urn:ietf:params:rtp-hdrext:ssrc-audio-level` extension (RFC 6464).
// LiveKit's SFU derives ActiveSpeakerUpdate from this extension; without
// it VoiceBlender is permanently silent from the server's POV and the
// browser-side `room.activeSpeakers` set never contains its identity.
type audioLevelInterceptor struct {
	interceptor.NoOp
	provider audioLevelProvider
}

func newAudioLevelInterceptor(p audioLevelProvider) *audioLevelInterceptor {
	return &audioLevelInterceptor{provider: p}
}

// NewInterceptor satisfies interceptor.Factory so the same value can be
// added to an interceptor.Registry directly.
func (a *audioLevelInterceptor) NewInterceptor(_ string) (interceptor.Interceptor, error) {
	return a, nil
}

// BindLocalStream resolves the negotiated extension ID once per stream
// and returns a writer that injects the level on every packet. If the
// stream never negotiated the extension (e.g. non-audio, or peer didn't
// offer it), we return the underlying writer unchanged.
func (a *audioLevelInterceptor) BindLocalStream(info *interceptor.StreamInfo, writer interceptor.RTPWriter) interceptor.RTPWriter {
	var extID uint8
	for _, ext := range info.RTPHeaderExtensions {
		if ext.URI == sdp.AudioLevelURI {
			extID = uint8(ext.ID)
			break
		}
	}
	if extID == 0 {
		return writer
	}
	return interceptor.RTPWriterFunc(func(header *rtp.Header, payload []byte, attrs interceptor.Attributes) (int, error) {
		level, voice := a.provider.AudioLevel()
		ext := rtp.AudioLevelExtension{Level: level, Voice: voice}
		if buf, err := ext.Marshal(); err == nil {
			_ = header.SetExtension(extID, buf)
		}
		return writer.Write(header, payload, attrs)
	})
}

// audioLevelFromPCM computes the RFC 6464 audio level (0 = full scale,
// 127 = silence) and a voice-activity hint from an int16 PCM frame.
// Voice activity is approximated as "level loud enough to be speech"
// (level < voiceLevelThreshold), since VoiceBlender doesn't carry an
// upstream VAD signal.
func audioLevelFromPCM(samples []int16) (uint8, bool) {
	const voiceLevelThreshold uint8 = 50
	if len(samples) == 0 {
		return 127, false
	}
	var sum int64
	for _, s := range samples {
		v := int64(s)
		sum += v * v
	}
	mean := float64(sum) / float64(len(samples))
	if mean < 1 {
		return 127, false
	}
	rms := math.Sqrt(mean)
	db := 20 * math.Log10(rms/32767)
	if db > 0 {
		db = 0
	}
	if db < -127 {
		db = -127
	}
	level := uint8(-db)
	return level, level < voiceLevelThreshold
}
