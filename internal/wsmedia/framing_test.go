package wsmedia

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"testing"

	"github.com/gobwas/ws"
)

func TestBinaryS16LERoundTrip(t *testing.T) {
	c := binaryS16LE{}
	in := []int16{0, 1, -1, 32767, -32768, 1234}
	payload, op, err := c.Encode(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if op != ws.OpBinary {
		t.Fatalf("want OpBinary, got %v", op)
	}
	out, err := c.Decode(ws.OpBinary, payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != len(in) {
		t.Fatalf("length mismatch: in=%d out=%d", len(in), len(out))
	}
	for i := range in {
		if in[i] != out[i] {
			t.Fatalf("sample %d: in=%d out=%d", i, in[i], out[i])
		}
	}
}

func TestBinaryS16LERejectsOddLength(t *testing.T) {
	if _, err := (binaryS16LE{}).Decode(ws.OpBinary, []byte{1, 2, 3}); err == nil {
		t.Fatal("want error on odd-length payload")
	}
}

func TestBinaryS16LEIgnoresText(t *testing.T) {
	out, err := (binaryS16LE{}).Decode(ws.OpText, []byte(`{"type":"text"}`))
	if err != nil {
		t.Fatalf("decode text: %v", err)
	}
	if out != nil {
		t.Fatalf("want nil samples for text frame, got %d", len(out))
	}
}

func TestJSONBase64Encode(t *testing.T) {
	c := jsonBase64S16LE{}
	in := []int16{1, 2, 3}
	payload, op, err := c.Encode(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if op != ws.OpText {
		t.Fatalf("want OpText, got %v", op)
	}
	var f jsonAudioFrame
	if err := json.Unmarshal(payload, &f); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if f.Type != "audio" {
		t.Fatalf("type=%q", f.Type)
	}
	raw, err := base64.StdEncoding.DecodeString(f.Audio)
	if err != nil {
		t.Fatalf("base64: %v", err)
	}
	if len(raw) != len(in)*2 {
		t.Fatalf("raw length %d, want %d", len(raw), len(in)*2)
	}
	for i, s := range in {
		got := int16(binary.LittleEndian.Uint16(raw[i*2:]))
		if got != s {
			t.Fatalf("sample %d: want %d, got %d", i, s, got)
		}
	}
}

func TestDecodeBase64Audio(t *testing.T) {
	in := []int16{10, 20, 30, -40}
	raw := pcmToBytes(in)
	b64 := base64.StdEncoding.EncodeToString(raw)
	out, err := decodeBase64Audio(b64)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != len(in) {
		t.Fatalf("length mismatch")
	}
	for i := range in {
		if in[i] != out[i] {
			t.Fatalf("sample %d: in=%d out=%d", i, in[i], out[i])
		}
	}
}

func TestParseControlFrame(t *testing.T) {
	cf, err := parseControlFrame([]byte(`{"type":"text","text":"hello","event_id":7}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cf.Type != "text" || cf.Text != "hello" || cf.EventID != 7 {
		t.Fatalf("unexpected: %+v", cf)
	}
}

func TestCodecFromConfig(t *testing.T) {
	tests := []struct {
		wire    WireFormat
		sample  SampleFormat
		wantErr bool
	}{
		{WireBinary, SampleS16LE, false},
		{WireJSONBase64, SampleS16LE, false},
		{WireFormat("nope"), SampleS16LE, true},
	}
	for _, tc := range tests {
		_, err := CodecFromConfig(Config{WireFormat: tc.wire, SampleFormat: tc.sample})
		if (err != nil) != tc.wantErr {
			t.Fatalf("wire=%q sample=%q: err=%v wantErr=%v", tc.wire, tc.sample, err, tc.wantErr)
		}
	}
}
