package codec

import (
	"fmt"
	"strings"
)

// CodecType identifies a supported audio codec.
type CodecType int

const (
	CodecUnknown CodecType = iota
	CodecPCMU                       // PT=0, 8kHz
	CodecPCMA                       // PT=8, 8kHz
	CodecG722                       // PT=9, 16kHz internal / 8kHz SDP clock (RFC 3551)
	CodecOpus                       // PT=111 (dynamic), 48kHz
)

func (c CodecType) String() string {
	switch c {
	case CodecPCMU:
		return "PCMU"
	case CodecPCMA:
		return "PCMA"
	case CodecG722:
		return "G722"
	case CodecOpus:
		return "opus"
	default:
		return "unknown"
	}
}

// PayloadType returns the RTP payload type number for this codec.
func (c CodecType) PayloadType() uint8 {
	switch c {
	case CodecPCMU:
		return 0
	case CodecPCMA:
		return 8
	case CodecG722:
		return 9
	case CodecOpus:
		return 111
	default:
		return 0
	}
}

// ClockRate returns the SDP clock rate.
// Per RFC 3551, G.722 uses 8000 in SDP despite encoding at 16kHz.
func (c CodecType) ClockRate() int {
	switch c {
	case CodecPCMU, CodecPCMA, CodecG722:
		return 8000
	case CodecOpus:
		return 48000
	default:
		return 8000
	}
}

// SampleRate returns the actual internal sample rate.
func (c CodecType) SampleRate() int {
	switch c {
	case CodecPCMU, CodecPCMA:
		return 8000
	case CodecG722:
		return 16000
	case CodecOpus:
		return 48000
	default:
		return 8000
	}
}

// CodecTypeFromPT looks up a CodecType by RTP payload type number.
func CodecTypeFromPT(pt uint8) CodecType {
	switch pt {
	case 0:
		return CodecPCMU
	case 8:
		return CodecPCMA
	case 9:
		return CodecG722
	case 111:
		return CodecOpus
	default:
		return CodecUnknown
	}
}

// CodecTypeFromName looks up a CodecType by name (case-insensitive).
func CodecTypeFromName(name string) CodecType {
	switch strings.ToUpper(name) {
	case "PCMU":
		return CodecPCMU
	case "PCMA":
		return CodecPCMA
	case "G722":
		return CodecG722
	case "OPUS":
		return CodecOpus
	default:
		return CodecUnknown
	}
}

// Encoder encodes PCM samples to compressed codec data.
type Encoder interface {
	Encode(samples []int16) ([]byte, error)
	Reset()
}

// Decoder decodes compressed codec data to PCM samples.
type Decoder interface {
	Decode(data []byte) ([]int16, error)
	Reset()
}

// NewEncoder creates an Encoder for the given codec type.
func NewEncoder(ct CodecType) (Encoder, error) {
	switch ct {
	case CodecPCMU:
		return &PCMUEncoder{}, nil
	case CodecPCMA:
		return &PCMAEncoder{}, nil
	case CodecG722:
		return NewG722Encoder(), nil
	case CodecOpus:
		return NewOpusEncoder()
	default:
		return nil, fmt.Errorf("unsupported encoder codec: %s", ct)
	}
}

// NewDecoder creates a Decoder for the given codec type.
func NewDecoder(ct CodecType) (Decoder, error) {
	switch ct {
	case CodecPCMU:
		return &PCMUDecoder{}, nil
	case CodecPCMA:
		return &PCMADecoder{}, nil
	case CodecG722:
		return NewG722Decoder(), nil
	case CodecOpus:
		return NewOpusDecoder()
	default:
		return nil, fmt.Errorf("unsupported decoder codec: %s", ct)
	}
}
