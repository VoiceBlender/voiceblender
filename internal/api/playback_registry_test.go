package api

import (
	"io"
	"testing"
	"time"
)

// clearLegPlayers registers a cleanup that drops every player registered for
// legID. legPlayers is a package-level global, so a test that seeds it must
// clean up or it leaks into every other test in the package.
func clearLegPlayers(t *testing.T, legID string) {
	t.Helper()
	t.Cleanup(func() {
		legPlayers.Lock()
		delete(legPlayers.m, legID)
		legPlayers.Unlock()
	})
}

func legPlayerCount(legID string) int {
	legPlayers.Lock()
	defer legPlayers.Unlock()
	return len(legPlayers.m[legID])
}

// TestStartLegPlay_UnknownToneDeregistersPlayer pins the invariant that every
// terminal branch of the playback goroutine clears its legPlayers entry. The
// unknown-tone branch returns early, before the normal deregistration, so
// without an explicit deregister the entry survives forever — a permanent leak
// for a leg that is already gone, and a stall for anything that waits on the
// leg having no audio outstanding.
func TestStartLegPlay_UnknownToneDeregistersPlayer(t *testing.T) {
	s := newTestServer(t)
	const legID = "leg-bad-tone"
	clearLegPlayers(t, legID)

	l := &apiMockLeg{id: legID, createdAt: time.Now(), audioWriter: io.Discard}
	s.LegMgr.Add(l)

	res, err := s.doStartLegPlay(legID, PlaybackRequest{Tone: "definitely-not-a-tone"})
	if err != nil {
		t.Fatalf("doStartLegPlay: %v", err)
	}
	if res.PlaybackID == "" {
		t.Fatal("no playback id returned")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if legPlayerCount(legID) == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("unknown tone left %d player(s) registered for %s", legPlayerCount(legID), legID)
}
