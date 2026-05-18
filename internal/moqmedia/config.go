// Package moqmedia provides a WebTransport+MoQ transport that carries
// bidirectional Opus audio for a single VoiceBlender leg. One MoQ session
// per WebTransport connection; the server publishes a `mix` namespace
// (downlink) and subscribes to the client's `mic` namespace (uplink).
package moqmedia

import (
	"errors"
	"fmt"
	"log/slog"
)

// Namespace and track names used on both directions. Kept fixed for the
// PoC: one MoQ session per leg, so there's no risk of namespace clashes.
const (
	MixNamespace = "mix"
	MicNamespace = "mic"
	AudioTrack   = "audio"
)

// Defaults applied by Config.Validate when zero values are supplied.
const (
	DefaultSampleRate      = 48000
	DefaultFrameMs         = 20
	DefaultOpusBitrate     = 24000
	DefaultIngressBufferMs = 1000
)

// Config configures a Transport. SampleRate is fixed at 48 kHz because
// gopus's encoder/decoder are constructed at 48 kHz mono; resampling to
// the room rate is the room's responsibility.
type Config struct {
	SampleRate      int
	FrameMs         int
	OpusBitrate     int
	IngressBufferMs int
	Log             *slog.Logger
}

func (c *Config) Validate() error {
	if c.Log == nil {
		return errors.New("moqmedia: Log is required")
	}
	if c.SampleRate == 0 {
		c.SampleRate = DefaultSampleRate
	}
	if c.SampleRate != 48000 {
		return fmt.Errorf("moqmedia: SampleRate must be 48000 (got %d)", c.SampleRate)
	}
	if c.FrameMs == 0 {
		c.FrameMs = DefaultFrameMs
	}
	if c.FrameMs <= 0 || 1000%c.FrameMs != 0 {
		return fmt.Errorf("moqmedia: FrameMs %d must divide 1000 evenly", c.FrameMs)
	}
	if c.OpusBitrate == 0 {
		c.OpusBitrate = DefaultOpusBitrate
	}
	if c.OpusBitrate < 6000 || c.OpusBitrate > 510000 {
		return fmt.Errorf("moqmedia: OpusBitrate %d out of range 6000..510000", c.OpusBitrate)
	}
	if c.IngressBufferMs == 0 {
		c.IngressBufferMs = DefaultIngressBufferMs
	}
	return nil
}

// FrameSamples returns the number of PCM samples per frame.
func (c *Config) FrameSamples() int { return c.SampleRate * c.FrameMs / 1000 }

// FrameBytesPCM returns the number of bytes per frame in the internal
// PCM16-LE representation.
func (c *Config) FrameBytesPCM() int { return c.FrameSamples() * 2 }

// IngressBufferBytes is the byte capacity of the audio ingress buffer.
func (c *Config) IngressBufferBytes() int {
	frames := c.IngressBufferMs / c.FrameMs
	if frames < 1 {
		frames = 1
	}
	return frames * c.FrameBytesPCM()
}
