package wsmedia

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"

	"github.com/gobwas/ws"
)

// AudioCodec converts between mixer-facing PCM16 frames and the on-the-wire
// representation. Implementations are stateless and safe for concurrent use.
type AudioCodec interface {
	// Encode converts a PCM16 frame to a WS payload + opcode.
	Encode(pcm []int16) (payload []byte, opCode ws.OpCode, err error)
	// Decode converts a received WS frame back to PCM16 samples. Returns
	// nil samples (no error) for frames the codec doesn't recognize as
	// audio (e.g. a text frame seen by a binary codec).
	Decode(opCode ws.OpCode, payload []byte) (pcm []int16, err error)
	// WireOpCode is the opcode the codec produces when encoding audio.
	WireOpCode() ws.OpCode
}

// CodecFromConfig returns the AudioCodec selected by the Config.
func CodecFromConfig(c Config) (AudioCodec, error) {
	switch c.WireFormat {
	case WireBinary:
		switch c.SampleFormat {
		case SampleS16LE:
			return binaryS16LE{}, nil
		default:
			return nil, fmt.Errorf("wsmedia: no codec for binary+%s", c.SampleFormat)
		}
	case WireJSONBase64:
		switch c.SampleFormat {
		case SampleS16LE:
			return jsonBase64S16LE{}, nil
		default:
			return nil, fmt.Errorf("wsmedia: no codec for json_base64+%s", c.SampleFormat)
		}
	default:
		return nil, fmt.Errorf("wsmedia: unsupported wire format %q", c.WireFormat)
	}
}

type binaryS16LE struct{}

func (binaryS16LE) WireOpCode() ws.OpCode { return ws.OpBinary }

func (binaryS16LE) Encode(pcm []int16) ([]byte, ws.OpCode, error) {
	out := make([]byte, len(pcm)*2)
	for i, s := range pcm {
		binary.LittleEndian.PutUint16(out[i*2:], uint16(s))
	}
	return out, ws.OpBinary, nil
}

func (binaryS16LE) Decode(op ws.OpCode, payload []byte) ([]int16, error) {
	if op != ws.OpBinary {
		return nil, nil
	}
	if len(payload)%2 != 0 {
		return nil, fmt.Errorf("wsmedia: binary s16le frame has odd length %d", len(payload))
	}
	out := make([]int16, len(payload)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(payload[i*2:]))
	}
	return out, nil
}

type jsonBase64S16LE struct{}

func (jsonBase64S16LE) WireOpCode() ws.OpCode { return ws.OpText }

type jsonAudioFrame struct {
	Type  string `json:"type"`
	Audio string `json:"audio"`
}

func (jsonBase64S16LE) Encode(pcm []int16) ([]byte, ws.OpCode, error) {
	raw := make([]byte, len(pcm)*2)
	for i, s := range pcm {
		binary.LittleEndian.PutUint16(raw[i*2:], uint16(s))
	}
	enc, err := json.Marshal(jsonAudioFrame{Type: "audio", Audio: base64.StdEncoding.EncodeToString(raw)})
	if err != nil {
		return nil, 0, err
	}
	return enc, ws.OpText, nil
}

// Decode for the JSON codec is a no-op because audio arrives interleaved
// with control text frames; the transport layer parses control envelopes
// and hands the base64 payload off to decodeBase64Audio directly.
func (jsonBase64S16LE) Decode(_ ws.OpCode, _ []byte) ([]int16, error) {
	return nil, nil
}

// decodeBase64Audio converts a base64-encoded PCM16-LE blob to []int16.
func decodeBase64Audio(b64 string) ([]int16, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("wsmedia: base64 audio: %w", err)
	}
	if len(raw)%2 != 0 {
		return nil, fmt.Errorf("wsmedia: base64 audio has odd length %d", len(raw))
	}
	out := make([]int16, len(raw)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(raw[i*2:]))
	}
	return out, nil
}

// pcmToBytes copies a PCM16 frame into a byte slice (LE).
func pcmToBytes(pcm []int16) []byte {
	out := make([]byte, len(pcm)*2)
	for i, s := range pcm {
		binary.LittleEndian.PutUint16(out[i*2:], uint16(s))
	}
	return out
}

// bytesToPCM is the inverse of pcmToBytes.
func bytesToPCM(b []byte) []int16 {
	out := make([]int16, len(b)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(b[i*2:]))
	}
	return out
}

// controlFrame is the JSON envelope shared by both wire formats for text and
// lifecycle messages. Audio frames are only encoded as JSON when WireFormat
// is WireJSONBase64; for WireBinary they go on the wire as raw binary frames.
type controlFrame struct {
	Type    string          `json:"type"`
	Audio   string          `json:"audio,omitempty"`
	Text    string          `json:"text,omitempty"`
	EventID int64           `json:"event_id,omitempty"`
	Raw     json.RawMessage `json:"-"`
}

func parseControlFrame(payload []byte) (controlFrame, error) {
	var cf controlFrame
	if err := json.Unmarshal(payload, &cf); err != nil {
		return cf, fmt.Errorf("wsmedia: parse control frame: %w", err)
	}
	cf.Raw = json.RawMessage(payload)
	return cf, nil
}
