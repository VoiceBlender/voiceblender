package api

import (
	"encoding/json"
	"testing"
)

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

// event_id is the deprecated alias for seq and must keep carrying the same
// counter until it is removed.
func TestVSIPingFrame(t *testing.T) {
	var m map[string]interface{}
	if err := json.Unmarshal(vsiPingFrame(7), &m); err != nil {
		t.Fatalf("unmarshal ping frame: %v", err)
	}
	if m["type"] != "ping" {
		t.Errorf("type = %v, want ping", m["type"])
	}
	if m["seq"] != float64(7) {
		t.Errorf("seq = %v, want 7", m["seq"])
	}
	if m["event_id"] != float64(7) {
		t.Errorf("event_id = %v, want 7", m["event_id"])
	}
}
