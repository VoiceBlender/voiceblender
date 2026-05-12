// Package wsmedia provides a reusable WebSocket transport for bidirectional
// PCM audio plus text and control messages. It is consumed by the WebSocket
// leg type and is shaped so that vendor-specific Agent providers can later
// reuse the same plumbing.
package wsmedia

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/wsutilx"
)

// WireFormat selects how audio frames are framed on the WebSocket.
type WireFormat string

const (
	// WireBinary sends PCM as raw WebSocket binary frames. Most efficient;
	// used by Deepgram, VAPI, Pipecat-style providers.
	WireBinary WireFormat = "binary"
	// WireJSONBase64 wraps PCM in {"type":"audio","audio":"<b64>"} text
	// frames. Matches the existing /v1/rooms/{id}/ws shape; browser-friendly.
	WireJSONBase64 WireFormat = "json_base64"
)

// SampleFormat selects the on-the-wire PCM sample encoding. v1 only ships
// signed 16-bit little-endian; the type is enum-shaped so 8-bit / companded
// formats can be added later without API churn.
type SampleFormat string

const (
	SampleS16LE SampleFormat = "s16le"
)

// Defaults applied by Config.Validate when zero values are supplied.
const (
	DefaultSampleRate      = 16000
	DefaultFrameMs         = 20
	DefaultWriteTimeout    = 5 * time.Second
	DefaultPingInterval    = 30 * time.Second
	DefaultIngressBufferMs = 1000
	DefaultTextBufferDepth = 50
)

// Config configures a Transport. Both DialClient and UpgradeServer take this
// struct; required vs optional is documented per field.
type Config struct {
	// SampleRate is the leg-native PCM rate. Must be one of 8000, 16000,
	// 24000, 48000.
	SampleRate int
	// SampleFormat selects the on-the-wire PCM encoding. Defaults to s16le.
	SampleFormat SampleFormat
	// WireFormat selects framing. Defaults to WireBinary.
	WireFormat WireFormat
	// FrameMs is the per-frame duration in milliseconds. Defaults to 20
	// (matches the mixer ptime).
	FrameMs int
	// ReadTimeout bounds inter-frame idle on the WS recv side. Defaults to
	// wsutilx.DefaultReadTimeout (~60s).
	ReadTimeout time.Duration
	// WriteTimeout bounds how long a single WS write may block on TCP
	// backpressure before being failed. Defaults to 5s.
	WriteTimeout time.Duration
	// PingInterval is how often the server-side helper sends control pings.
	// Defaults to 30s.
	PingInterval time.Duration
	// IngressBufferMs caps how much inbound audio may be buffered before
	// the recv loop starts dropping frames. Defaults to 1000ms.
	IngressBufferMs int
	// TextBufferDepth caps how many inbound text messages may be buffered
	// before the recv loop starts dropping. Defaults to 50.
	TextBufferDepth int
	// Headers carries HTTP headers either to send (DialClient) or already
	// observed (UpgradeServer surfaces these via its return value).
	Headers http.Header
	// TextEnabled controls whether inbound and outbound text frames are
	// processed. Defaults to true.
	TextEnabled *bool
	// Log receives structured diagnostics. Required.
	Log *slog.Logger
}

// Validate applies defaults and reports any unrecoverable misconfiguration.
// Callers should invoke this before DialClient/UpgradeServer.
func (c *Config) Validate() error {
	if c.Log == nil {
		return errors.New("wsmedia: Log is required")
	}
	if c.SampleRate == 0 {
		c.SampleRate = DefaultSampleRate
	}
	switch c.SampleRate {
	case 8000, 16000, 24000, 48000:
	default:
		return fmt.Errorf("wsmedia: invalid SampleRate %d (want 8000/16000/24000/48000)", c.SampleRate)
	}
	if c.SampleFormat == "" {
		c.SampleFormat = SampleS16LE
	}
	if c.SampleFormat != SampleS16LE {
		return fmt.Errorf("wsmedia: unsupported SampleFormat %q", c.SampleFormat)
	}
	if c.WireFormat == "" {
		c.WireFormat = WireBinary
	}
	if c.WireFormat != WireBinary && c.WireFormat != WireJSONBase64 {
		return fmt.Errorf("wsmedia: unsupported WireFormat %q", c.WireFormat)
	}
	if c.FrameMs == 0 {
		c.FrameMs = DefaultFrameMs
	}
	if c.FrameMs <= 0 || 1000%c.FrameMs != 0 {
		return fmt.Errorf("wsmedia: FrameMs %d must divide 1000 evenly", c.FrameMs)
	}
	if c.ReadTimeout == 0 {
		c.ReadTimeout = wsutilx.DefaultReadTimeout.Load()
	}
	if c.WriteTimeout == 0 {
		c.WriteTimeout = DefaultWriteTimeout
	}
	if c.PingInterval == 0 {
		c.PingInterval = DefaultPingInterval
	}
	if c.IngressBufferMs == 0 {
		c.IngressBufferMs = DefaultIngressBufferMs
	}
	if c.TextBufferDepth == 0 {
		c.TextBufferDepth = DefaultTextBufferDepth
	}
	if c.TextEnabled == nil {
		t := true
		c.TextEnabled = &t
	}
	return nil
}

// FrameSamples returns the number of PCM samples per frame.
func (c *Config) FrameSamples() int { return c.SampleRate * c.FrameMs / 1000 }

// FrameBytesPCM returns the number of bytes per frame in the internal PCM16
// representation (mixer-facing) regardless of wire SampleFormat.
func (c *Config) FrameBytesPCM() int { return c.FrameSamples() * 2 }

// IngressBufferBytes is the byte capacity of the audio ingress streamBuffer.
func (c *Config) IngressBufferBytes() int {
	frames := c.IngressBufferMs / c.FrameMs
	if frames < 1 {
		frames = 1
	}
	return frames * c.FrameBytesPCM()
}

// TextEnabledValue dereferences TextEnabled with a true default. Use after
// Validate.
func (c *Config) TextEnabledValue() bool {
	if c.TextEnabled == nil {
		return true
	}
	return *c.TextEnabled
}
