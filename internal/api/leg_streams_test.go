package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/leg"
)

func TestLegStreamView_SRTP(t *testing.T) {
	on, err := json.Marshal(toLegStreamView(leg.StreamInfo{ID: "0", SRTP: true}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(on), `"srtp":true`) {
		t.Errorf("an encrypted stream's view lacks srtp: %s", on)
	}
	off, err := json.Marshal(toLegStreamView(leg.StreamInfo{ID: "0"}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(off), "srtp") {
		t.Errorf("a plain stream's view must omit srtp: %s", off)
	}
}
