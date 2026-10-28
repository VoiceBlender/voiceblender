package t140

import (
	"strings"
	"testing"
)

// A redundant block older than the 14-bit RED timestamp offset (16383 ms)
// cannot be described correctly. It must not be sent with a clamped offset,
// because the receiver would reconstruct the wrong timestamp, miss its dedup
// entry and print the old text a second time.
func TestEncoderStaleHistoryNotResent(t *testing.T) {
	enc := NewEncoder(2, 99)
	dec := NewDecoder()

	send := func(seq uint16, ts uint32, text string) string {
		enc.Push(text)
		pl, useRED := enc.Flush(ts)
		if !useRED {
			t.Fatalf("seq %d: expected RED", seq)
		}
		got, _, err := dec.DecodePacket(seq, ts, 98, 99, 98, pl)
		if err != nil {
			t.Fatalf("seq %d: decode err: %v", seq, err)
		}
		return got
	}

	if got := send(1, 1000, "hello"); got != "hello" {
		t.Fatalf("seq1: got %q want hello", got)
	}
	// User pauses for 20 s, then types again.
	if got := send(2, 1000+20000, "x"); got != "x" {
		t.Fatalf("after idle gap: got %q want %q (old text re-emitted)", got, "x")
	}
}

// A chunk longer than the 10-bit RED block length (1023 bytes) cannot be
// carried as redundancy. Clamping only the header length while appending the
// full body desynchronises the receiver's parser and corrupts the primary.
func TestEncoderOversizedHistoryChunk(t *testing.T) {
	enc := NewEncoder(1, 99)
	dec := NewDecoder()

	big := strings.Repeat("a", 1500)
	enc.Push(big)
	pl1, _ := enc.Flush(1000)
	if got, _, _ := dec.DecodePacket(1, 1000, 98, 99, 98, pl1); got != big {
		t.Fatalf("seq1: got %d bytes want %d", len(got), len(big))
	}

	enc.Push("B")
	pl2, _ := enc.Flush(1300)
	got, _, err := dec.DecodePacket(2, 1300, 98, 99, 98, pl2)
	if err != nil {
		t.Fatalf("seq2: decode err: %v", err)
	}
	if got != "B" {
		t.Fatalf("seq2: got %q (len %d) want %q", trunc(got), len(got), "B")
	}
}

func trunc(s string) string {
	if len(s) > 20 {
		return s[:10] + "…" + s[len(s)-10:]
	}
	return s
}
