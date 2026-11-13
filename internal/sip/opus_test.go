package sip

import (
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/codec"
)

func TestOpusSendParams(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fmtp    string
		bitrate int
		want    codec.OpusParams
	}{
		{"no fmtp", "", 24000, codec.OpusParams{Bitrate: 24000}},
		{"unset bitrate is the default", "", 0, codec.OpusParams{Bitrate: codec.DefaultOpusBitrate}},
		{"configured bitrate clamped", "", 1000000, codec.OpusParams{Bitrate: codec.MaxOpusBitrate}},
		{"other params ignored", "minptime=10;useinbandfec=1", 32000, codec.OpusParams{Bitrate: 32000}},
		{"peer caps the bitrate", "minptime=10; maxaveragebitrate=12000", 24000, codec.OpusParams{Bitrate: 12000}},
		{"peer allows more than configured", "maxaveragebitrate=64000", 24000, codec.OpusParams{Bitrate: 24000}},
		{"peer cap below the floor", "maxaveragebitrate=2000", 24000, codec.OpusParams{Bitrate: codec.MinOpusBitrate}},
		{"playback rate", "maxplaybackrate=16000; sprop-maxcapturerate=16000", 24000, codec.OpusParams{Bitrate: 24000, MaxPlaybackRate: 16000}},
		{"both, case-insensitive", "MaxAverageBitrate=20000;MAXPLAYBACKRATE=8000", 24000, codec.OpusParams{Bitrate: 20000, MaxPlaybackRate: 8000}},
		{"malformed values ignored", "maxaveragebitrate=abc; maxplaybackrate=-1", 24000, codec.OpusParams{Bitrate: 24000}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := OpusSendParams(tc.fmtp, tc.bitrate); got != tc.want {
				t.Errorf("OpusSendParams(%q, %d) = %+v, want %+v", tc.fmtp, tc.bitrate, got, tc.want)
			}
		})
	}
}
