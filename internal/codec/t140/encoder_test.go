package t140

import (
	"bytes"
	"strings"
	"testing"
)

func TestEncoderPlainT140(t *testing.T) {
	enc := NewEncoder(0, DefaultT140PT)
	enc.Push("hi")
	pl, useRED := enc.Flush(1000)
	if useRED {
		t.Fatalf("redundancy=0 must not produce RED")
	}
	if !bytes.Equal(pl, []byte("hi")) {
		t.Fatalf("plain T.140 payload mismatch: got %q", pl)
	}
}

func TestEncoderEmptyFlushNoRed(t *testing.T) {
	enc := NewEncoder(2, DefaultT140PT)
	pl, useRED := enc.Flush(1000)
	if pl != nil || useRED {
		t.Fatalf("empty flush with no history must return nil, false")
	}
}

func TestEncoderFirstPacketRedWrappedNoRedundancy(t *testing.T) {
	enc := NewEncoder(2, DefaultT140PT)
	enc.Push("a")
	pl, useRED := enc.Flush(1000)
	if !useRED {
		t.Fatalf("first packet with redundancy>0 must be RED-wrapped (RFC 4103 §4.3)")
	}
	// Layout: primary header (1B, F=0|PT) followed by primary body.
	if len(pl) != 2 {
		t.Fatalf("expected 2-byte payload, got %d", len(pl))
	}
	if pl[0] != DefaultT140PT&0x7F {
		t.Fatalf("primary header: got 0x%02X want 0x%02X", pl[0], DefaultT140PT&0x7F)
	}
	if pl[1] != 'a' {
		t.Fatalf("primary body: got %q want 'a'", pl[1:])
	}
}

func TestEncoderRedundancyHeaders(t *testing.T) {
	enc := NewEncoder(2, 99)
	// First packet: plain T.140 (no history).
	enc.Push("A")
	enc.Flush(1000)
	// Second packet: should carry one redundant block (first chunk).
	enc.Push("B")
	pl, useRED := enc.Flush(1300)
	if !useRED {
		t.Fatalf("second packet with history must be RED")
	}
	// Layout: 1 redundant header (4B) + 1 primary header (1B) + bodies.
	if len(pl) < 5 {
		t.Fatalf("RED payload too short: %d bytes", len(pl))
	}
	// First header: F=1, PT=99 → 0x80 | 0x63 = 0xE3
	if pl[0] != 0xE3 {
		t.Fatalf("redundant header byte 0: got 0x%02X, want 0xE3", pl[0])
	}
	// Primary header at offset 4: F=0, PT=99
	if pl[4] != 0x63 {
		t.Fatalf("primary header: got 0x%02X, want 0x63", pl[4])
	}
	// Bodies: redundant 'A' then primary 'B'.
	if !bytes.Equal(pl[5:], []byte("AB")) {
		t.Fatalf("bodies: got %q, want %q", pl[5:], "AB")
	}
}

func TestEncoderHistoryCapped(t *testing.T) {
	enc := NewEncoder(2, 99)
	for i, ch := range []string{"A", "B", "C", "D"} {
		enc.Push(ch)
		enc.Flush(uint32(1000 + i*300))
	}
	// History should now hold the last 2 chunks: C, D... but D is the just-
	// flushed primary, which is also pushed onto history, so history holds
	// the two most recent primaries: C and D.
	if got := len(enc.history); got != 2 {
		t.Fatalf("history length: got %d, want 2", got)
	}
	if string(enc.history[0].data) != "C" || string(enc.history[1].data) != "D" {
		t.Fatalf("history contents: got %q,%q want C,D", enc.history[0].data, enc.history[1].data)
	}
}

func TestEncoderDecodeRoundTrip(t *testing.T) {
	enc := NewEncoder(2, 99)
	dec := NewDecoder()

	send := func(seq uint16, ts uint32, text string) string {
		enc.Push(text)
		pl, useRED := enc.Flush(ts)
		pt := uint8(99)
		if useRED {
			pt = 98
		}
		got, _, err := dec.DecodePacket(seq, ts, pt, 99, 98, pl)
		if err != nil {
			t.Fatalf("decode err: %v", err)
		}
		return got
	}

	if got := send(1, 1000, "Hel"); got != "Hel" {
		t.Fatalf("seq1: got %q want Hel", got)
	}
	if got := send(2, 1300, "lo "); got != "lo " {
		t.Fatalf("seq2: got %q want 'lo '", got)
	}
	if got := send(3, 1600, "wor"); got != "wor" {
		t.Fatalf("seq3: got %q want wor", got)
	}
}

func TestEncoderLossRecoveredByRED(t *testing.T) {
	enc := NewEncoder(2, 99)
	dec := NewDecoder()

	enc.Push("A")
	pl1, _ := enc.Flush(1000)
	if got, _, _ := dec.DecodePacket(1, 1000, 98, 99, 98, pl1); got != "A" {
		t.Fatalf("seq1: got %q want A", got)
	}

	enc.Push("B")
	enc.Flush(1300) // packet 2 — DROPPED before reaching decoder

	enc.Push("C")
	pl3, useRED := enc.Flush(1600)
	if !useRED {
		t.Fatalf("packet 3 should be RED")
	}
	got, lost, _ := dec.DecodePacket(3, 1600, 98, 99, 98, pl3)
	if lost {
		t.Fatalf("RED redundancy=2 covers a single dropped packet — should NOT mark loss")
	}
	if got != "BC" {
		t.Fatalf("loss recovery: got %q want BC", got)
	}
}

func TestEncoderLossExceedsRedundancy(t *testing.T) {
	enc := NewEncoder(1, 99)
	dec := NewDecoder()

	enc.Push("A")
	pl1, _ := enc.Flush(1000)
	dec.DecodePacket(1, 1000, 99, 99, 98, pl1)

	// Drop packets 2 and 3.
	enc.Push("B")
	enc.Flush(1300)
	enc.Push("C")
	enc.Flush(1600)

	enc.Push("D")
	pl4, useRED := enc.Flush(1900)
	if !useRED {
		t.Fatalf("packet 4 should be RED")
	}
	got, lost, _ := dec.DecodePacket(4, 1900, 98, 99, 98, pl4)
	if !lost {
		t.Fatalf("loss exceeding redundancy must mark loss")
	}
	if !strings.HasPrefix(got, ReplacementChar) {
		t.Fatalf("loss marker missing: got %q", got)
	}
	if !strings.HasSuffix(got, "CD") {
		t.Fatalf("expected suffix CD: got %q", got)
	}
}

func TestEncoderDedupOutOfOrder(t *testing.T) {
	enc := NewEncoder(2, 99)
	dec := NewDecoder()

	enc.Push("A")
	pl1, _ := enc.Flush(1000)
	enc.Push("B")
	pl2, _ := enc.Flush(1300)

	if got, _, _ := dec.DecodePacket(2, 1300, 98, 99, 98, pl2); got != "AB" {
		t.Fatalf("packet 2: got %q want AB", got)
	}
	// Packet 1 arrives late: data should already have been emitted via RED.
	if got, _, _ := dec.DecodePacket(1, 1000, 98, 99, 98, pl1); got != "" {
		t.Fatalf("late packet 1: got %q want empty (already emitted)", got)
	}
}

func TestEncoderUTF8Multibyte(t *testing.T) {
	enc := NewEncoder(0, 99)
	dec := NewDecoder()

	enc.Push("zażółć gęślą jaźń")
	pl, _ := enc.Flush(1000)
	got, _, err := dec.DecodePacket(1, 1000, 99, 99, 98, pl)
	if err != nil {
		t.Fatalf("decode err: %v", err)
	}
	if got != "zażółć gęślą jaźń" {
		t.Fatalf("utf8 mismatch: got %q", got)
	}
}
