package peers

import (
	"errors"
	"testing"
)

func TestSelected(t *testing.T) {
	t.Setenv(EnvPeers, "")
	if got := Selected(); len(got) != 1 || got[0] != DefaultPeers {
		t.Errorf("default = %v", got)
	}
	t.Setenv(EnvPeers, " freeswitch, asterisk ,,")
	if got := Selected(); len(got) != 2 || got[0] != "freeswitch" || got[1] != "asterisk" {
		t.Errorf("parsed = %v", got)
	}
}

func TestGet_StartsOnceAndRemembersFailure(t *testing.T) {
	starts := 0
	Register("test-failing", func() (Peer, error) {
		starts++
		return nil, errors.New("boom")
	})
	for i := 0; i < 3; i++ {
		if _, err := Get("test-failing"); err == nil {
			t.Fatal("want the start error")
		}
	}
	if starts != 1 {
		t.Errorf("factory ran %d times, want 1", starts)
	}
	if _, err := Get("test-unknown"); err == nil {
		t.Error("unknown endpoint should fail")
	}
	ShutdownAll()
}

func TestCapsHasCodec(t *testing.T) {
	c := Caps{Codecs: []string{"PCMU", "OPUS"}}
	if !c.HasCodec("opus") || c.HasCodec("G722") {
		t.Errorf("HasCodec mismatch for %v", c.Codecs)
	}
}

func TestSIPCodeToQ850(t *testing.T) {
	for code, want := range map[int]string{486: "USER_BUSY", 603: "CALL_REJECTED", 404: "UNALLOCATED_NUMBER", 488: "INCOMPATIBLE_DESTINATION"} {
		if got := SIPCodeToQ850(code); got != want {
			t.Errorf("SIPCodeToQ850(%d) = %s, want %s", code, got, want)
		}
	}
}
