package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestVSIPingFrame_UsesSeq is the only executable proof that the VSI keepalive
// counter is named "seq" on the wire. It asserts on vsiPingFrame — the helper
// the ping loop actually calls — so reverting the emitted field name back to
// "event_id" reddens it. It must not restate the frame as a map literal, which
// would only assert on itself.
//
// The name matters: streamed events on this same socket now carry an "event_id"
// (the per-event idempotency key), so a ping using that name would advertise two
// different meanings for one field.
func TestVSIPingFrame_UsesSeq(t *testing.T) {
	raw := vsiPingFrame(1)

	if strings.Contains(string(raw), "event_id") {
		t.Errorf("ping frame must not mention event_id, got %s", raw)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal ping frame: %v", err)
	}
	if m["type"] != "ping" {
		t.Errorf("type = %v, want ping", m["type"])
	}
	seq, ok := m["seq"].(float64)
	if !ok {
		t.Fatalf("expected numeric seq field, got %#v (frame: %s)", m["seq"], raw)
	}
	if seq != 1 {
		t.Errorf("seq = %v, want 1", seq)
	}
	if _, ok := m["event_id"]; ok {
		t.Errorf("ping frame carries event_id: %s", raw)
	}
}

func TestIsDropLogThreshold(t *testing.T) {
	cases := []struct {
		n    int64
		want bool
	}{
		{0, false},
		{-1, false},
		{1, true},
		{2, false},
		{9, false},
		{10, true},
		{11, false},
		{99, false},
		{100, true},
		{500, false},
		{1000, true},
		{10000, true},
		{99999, false},
		{100000, true},
		{1_000_000, true},
	}
	for _, c := range cases {
		if got := isDropLogThreshold(c.n); got != c.want {
			t.Errorf("isDropLogThreshold(%d) = %v, want %v", c.n, got, c.want)
		}
	}
}
