package t140

import "testing"

func TestParseREDMalformed(t *testing.T) {
	cases := [][]byte{
		nil,
		{},
		{0x80, 0x00},                   // truncated redundant header
		{0x80, 0x00, 0x00, 0x04, 0x00}, // length=4 but only 1 body byte
	}
	for i, p := range cases {
		if _, err := parseRED(p, 0); err == nil {
			t.Fatalf("case %d: expected error", i)
		}
	}
}

func TestParseREDPrimaryOnly(t *testing.T) {
	pl := append([]byte{0x63}, []byte("hello")...) // F=0, PT=99, body
	blocks, err := parseRED(pl, 1000)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(blocks) != 1 {
		t.Fatalf("blocks: got %d want 1", len(blocks))
	}
	if blocks[0].ts != 1000 || string(blocks[0].data) != "hello" {
		t.Fatalf("block: %+v %q", blocks[0], blocks[0].data)
	}
}

func TestDecoderUnknownPTTreatedAsT140(t *testing.T) {
	dec := NewDecoder()
	got, _, err := dec.DecodePacket(1, 100, 200, 99, 0, []byte("xyz"))
	if err != nil {
		t.Fatalf("decode err: %v", err)
	}
	if got != "xyz" {
		t.Fatalf("got %q want xyz", got)
	}
}

func TestDecoderTimestampOffsetEncoding(t *testing.T) {
	enc := NewEncoder(1, 99)
	enc.Push("A")
	enc.Flush(1000)
	enc.Push("B")
	pl, useRED := enc.Flush(1300)
	if !useRED {
		t.Fatalf("expected RED")
	}
	// 14-bit ts offset = 300 → high 8 bits = 4, low 6 bits = 44.
	// header: F=1|PT=99 | (300>>6)=4 | ((300&63)<<2)|(len>>8) | len&0xFF
	// len(A)=1 → byte2 = (44<<2)|0 = 176, byte3 = 1.
	if pl[1] != 4 {
		t.Fatalf("ts offset hi byte: got %d want 4", pl[1])
	}
	if pl[2] != (44<<2)|0 {
		t.Fatalf("ts offset lo / len hi byte: got %d want %d", pl[2], (44<<2)|0)
	}
	if pl[3] != 1 {
		t.Fatalf("len lo byte: got %d want 1", pl[3])
	}
}
