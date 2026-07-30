package api

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/VoiceBlender/voiceblender/internal/playback"
)

// seedLegPlayer registers a player for legID without ever starting it: the
// drain's predicate is legPlayers membership, not audio actually flowing, so
// these tests stay sub-second and never depend on the media path.
func seedLegPlayer(t *testing.T, s *Server, legID, playbackID string) {
	t.Helper()
	clearLegPlayers(t, legID)
	legPlayers.Lock()
	if legPlayers.m[legID] == nil {
		legPlayers.m[legID] = make(map[string]*playback.Player)
	}
	legPlayers.m[legID][playbackID] = playback.NewPlayer(s.Log)
	legPlayers.Unlock()
}

// watchLegDisconnected returns a predicate reporting whether leg.disconnected
// has been published for legID.
func watchLegDisconnected(s *Server, legID string) func() bool {
	var mu sync.Mutex
	seen := false
	s.Bus.Subscribe(func(e events.Event) {
		if e.Type != events.LegDisconnected {
			return
		}
		d, ok := e.Data.(*events.LegDisconnectedData)
		if !ok || d.LegID != legID {
			return
		}
		mu.Lock()
		seen = true
		mu.Unlock()
	})
	return func() bool {
		mu.Lock()
		defer mu.Unlock()
		return seen
	}
}

func waitFor(t *testing.T, d time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", d, what)
}

// TestDoDeleteLeg_DrainWaitsForOutstandingPlayback is the feature: with
// drain_playback set, neither the BYE nor leg.disconnected happens while an
// utterance is still outstanding, and both happen once it deregisters.
func TestDoDeleteLeg_DrainWaitsForOutstandingPlayback(t *testing.T) {
	s := newTestServer(t)
	const legID = "leg-drain-waits"

	disconnected := watchLegDisconnected(s, legID)
	l := &apiMockLeg{id: legID, createdAt: time.Now()}
	s.LegMgr.Add(l)
	seedLegPlayer(t, s, legID, "pb-outstanding")

	if err := s.doDeleteLeg(legID, "", true, 2000); err != nil {
		t.Fatalf("doDeleteLeg: %v", err)
	}

	time.Sleep(300 * time.Millisecond)
	if disconnected() {
		t.Error("leg.disconnected published while playback was still outstanding")
	}
	if n := l.hangups.Load(); n != 0 {
		t.Errorf("Hangup called %d time(s) during the drain, want 0", n)
	}

	deregisterLegPlayer(legID, "pb-outstanding")
	waitFor(t, time.Second, disconnected, "leg.disconnected once the playback deregistered")
}

// TestDoDeleteLeg_DrainExpiryReleasesTeardown pins the bound: a player that
// never deregisters, on a leg whose context is never cancelled, still gets torn
// down once the budget elapses.
func TestDoDeleteLeg_DrainExpiryReleasesTeardown(t *testing.T) {
	s := newTestServer(t)
	const legID = "leg-drain-expiry"

	disconnected := watchLegDisconnected(s, legID)
	l := &apiMockLeg{id: legID, createdAt: time.Now()}
	s.LegMgr.Add(l)
	seedLegPlayer(t, s, legID, "pb-stuck")

	if err := s.doDeleteLeg(legID, "", true, 200); err != nil {
		t.Fatalf("doDeleteLeg: %v", err)
	}

	waitFor(t, 2*time.Second, disconnected, "leg.disconnected after the drain budget expired")
}

// TestDoDeleteLeg_NoDrainWithoutOptIn is the behaviour-preservation guard: a
// DELETE that does not ask for a drain must tear down immediately even with an
// utterance outstanding, exactly as it does today.
func TestDoDeleteLeg_NoDrainWithoutOptIn(t *testing.T) {
	s := newTestServer(t)
	const legID = "leg-no-drain"

	disconnected := watchLegDisconnected(s, legID)
	l := &apiMockLeg{id: legID, createdAt: time.Now()}
	s.LegMgr.Add(l)
	seedLegPlayer(t, s, legID, "pb-ignored")

	if err := s.doDeleteLeg(legID, "", false, 0); err != nil {
		t.Fatalf("doDeleteLeg: %v", err)
	}

	waitFor(t, time.Second, disconnected, "leg.disconnected without a drain opt-in")
}

// TestDrainLegPlayback_AbortsOnLegContextCancel proves the wait ends as soon as
// the leg context is cancelled — which is what happens when a competing
// teardown path reaches Hangup first — rather than burning the whole budget.
func TestDrainLegPlayback_AbortsOnLegContextCancel(t *testing.T) {
	s := newTestServer(t)
	const legID = "leg-drain-cancel"
	seedLegPlayer(t, s, legID, "pb-stuck")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan bool, 1)
	go func() {
		// The budget is far outside the assertion window, so the timer arm
		// cannot stand in for the missing context arm.
		done <- drainLegPlayback(ctx, legID, 30*time.Second)
	}()

	time.AfterFunc(50*time.Millisecond, cancel)
	select {
	case drained := <-done:
		if drained {
			t.Error("drainLegPlayback reported a drain after its context was cancelled")
		}
	case <-time.After(time.Second):
		t.Fatal("drainLegPlayback did not return after the leg context was cancelled")
	}
}

// TestDrainLegPlayback_FastPathsWhenIdle guards the common case: a leg with
// nothing outstanding must not wait at all.
func TestDrainLegPlayback_FastPathsWhenIdle(t *testing.T) {
	// A zero budget isolates the fast path from the first poll tick: without a
	// fast path the timer fires before the 10ms tick, and an idle leg would be
	// reported as having failed to drain.
	if !drainLegPlayback(context.Background(), "leg-never-played", 0) {
		t.Error("drainLegPlayback reported no drain for an idle leg on a zero budget")
	}

	start := time.Now()
	if !drainLegPlayback(context.Background(), "leg-never-played", time.Minute) {
		t.Error("drainLegPlayback reported no drain for a leg with no playback")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("idle leg took %s to drain, want an immediate return", elapsed)
	}
}

// TestDrainBudget_DefaultAndClamp checks normalisation at every boundary.
//
// The expectations are bare literals on purpose: referencing
// defaultDrainTimeout/maxDrainTimeout would make the table move with the
// constants and stop detecting a change to them.
func TestDrainBudget_DefaultAndClamp(t *testing.T) {
	tests := []struct {
		name string
		ms   int
		want time.Duration
	}{
		{"omitted", 0, 5 * time.Second},
		{"negative", -1, 5 * time.Second},
		{"explicit", 250, 250 * time.Millisecond},
		{"just under the ceiling", 29999, 29999 * time.Millisecond},
		{"exactly the ceiling", 30000, 30 * time.Second},
		{"over the ceiling", 30001, 30 * time.Second},
		// Clamping must happen on the millisecond count: converting first
		// overflows int64 into a duration of 0 (or a negative one), which
		// would fire the timer instantly and silently disable the drain.
		{"overflowing", 1 << 62, 30 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := drainBudget(tt.ms); got != tt.want {
				t.Errorf("drainBudget(%d) = %s, want %s", tt.ms, got, tt.want)
			}
		})
	}
}

func TestDrainablePlaybackState(t *testing.T) {
	tests := []struct {
		state leg.LegState
		want  bool
	}{
		{leg.StateConnected, true},
		{leg.StateEarlyMedia, true},
		{leg.StateHeld, false},
		{leg.StateRinging, false},
		{leg.StateHungUp, false},
	}
	for _, tt := range tests {
		if got := drainablePlaybackState(tt.state); got != tt.want {
			t.Errorf("drainablePlaybackState(%q) = %v, want %v", tt.state, got, tt.want)
		}
	}
}
