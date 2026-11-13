package sip

import (
	"strconv"
	"strings"

	"github.com/VoiceBlender/voiceblender/internal/codec"
)

// OpusSendParams returns the encoder settings for sending Opus to a receiver
// that advertised fmtp: the configured bitrate capped by its
// maxaveragebitrate, and its maxplaybackrate (RFC 7587 §6.1).
func OpusSendParams(fmtp string, bitrate int) codec.OpusParams {
	p := codec.OpusParams{Bitrate: clampOpusBitrate(bitrate)}
	if v, ok := opusFmtpInt(fmtp, "maxaveragebitrate"); ok {
		p.Bitrate = min(p.Bitrate, clampOpusBitrate(v))
	}
	if v, ok := opusFmtpInt(fmtp, "maxplaybackrate"); ok {
		p.MaxPlaybackRate = v
	}
	return p
}

func clampOpusBitrate(bps int) int {
	if bps <= 0 {
		return codec.DefaultOpusBitrate
	}
	return max(codec.MinOpusBitrate, min(bps, codec.MaxOpusBitrate))
}

// opusFmtpInt returns a positive integer fmtp parameter.
func opusFmtpInt(fmtp, name string) (int, bool) {
	for _, part := range strings.Split(fmtp, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), name) {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n <= 0 {
			return 0, false
		}
		return n, true
	}
	return 0, false
}
