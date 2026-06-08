package lkmedia

import (
	"errors"
	"fmt"
	"log/slog"
)

// Defaults applied by Config.Validate when zero values are supplied.
const (
	DefaultSampleRate      = 48000 // gopus encoder/decoder fixed at 48 kHz
	DefaultFrameMs         = 20    // standard Opus ptime
	DefaultOpusBitrate     = 24000 // sensible voice bitrate; LiveKit defaults similar
	DefaultIngressBufferMs = 1000  // bound for the sum-mixed PCM ingress buffer
	DefaultLanePCMBufferMs = 200   // per-LK-remote-track decoded PCM ring (drop-oldest)
)

// Config configures a Transport. SampleRate is fixed at 48 kHz because
// gopus's encoder/decoder are constructed at 48 kHz mono; resampling to
// the room rate is the room's responsibility.
type Config struct {
	SampleRate      int
	FrameMs         int
	OpusBitrate     int
	IngressBufferMs int
	LaneBufferMs    int
	Log             *slog.Logger
}

func (c *Config) Validate() error {
	if c.Log == nil {
		return errors.New("lkmedia: Log is required")
	}
	if c.SampleRate == 0 {
		c.SampleRate = DefaultSampleRate
	}
	if c.SampleRate != 48000 {
		return fmt.Errorf("lkmedia: SampleRate must be 48000 (got %d)", c.SampleRate)
	}
	if c.FrameMs == 0 {
		c.FrameMs = DefaultFrameMs
	}
	if c.FrameMs <= 0 || 1000%c.FrameMs != 0 {
		return fmt.Errorf("lkmedia: FrameMs %d must divide 1000 evenly", c.FrameMs)
	}
	if c.OpusBitrate == 0 {
		c.OpusBitrate = DefaultOpusBitrate
	}
	if c.OpusBitrate < 6000 || c.OpusBitrate > 510000 {
		return fmt.Errorf("lkmedia: OpusBitrate %d out of range 6000..510000", c.OpusBitrate)
	}
	if c.IngressBufferMs == 0 {
		c.IngressBufferMs = DefaultIngressBufferMs
	}
	if c.LaneBufferMs == 0 {
		c.LaneBufferMs = DefaultLanePCMBufferMs
	}
	return nil
}

// FrameSamples returns the number of PCM samples per frame.
func (c *Config) FrameSamples() int { return c.SampleRate * c.FrameMs / 1000 }

// FrameBytesPCM returns the number of bytes per frame in PCM16-LE.
func (c *Config) FrameBytesPCM() int { return c.FrameSamples() * 2 }

// IngressBufferBytes is the byte capacity of the sum-mixed output buffer
// that AudioReader drains.
func (c *Config) IngressBufferBytes() int {
	frames := c.IngressBufferMs / c.FrameMs
	if frames < 1 {
		frames = 1
	}
	return frames * c.FrameBytesPCM()
}

// LaneCapacityFrames is the per-remote-track ring capacity in frames.
func (c *Config) LaneCapacityFrames() int {
	frames := c.LaneBufferMs / c.FrameMs
	if frames < 2 {
		frames = 2
	}
	return frames
}
