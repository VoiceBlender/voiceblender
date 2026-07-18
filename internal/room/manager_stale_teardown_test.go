package room

import (
	"io"
	"sync"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/VoiceBlender/voiceblender/internal/mixer"
)

// addHealthyLeg joins a leg whose audio path never fails: its reader blocks
// until the pipe is written to, so the mixer's readLoop simply parks there.
func addHealthyLeg(t *testing.T, mgr *Manager, legMgr *leg.Manager, roomID, legID string) *mockLeg {
	t.Helper()
	pr, pw := io.Pipe()
	t.Cleanup(func() { pw.Close() })

	l := newMockLeg(legID)
	l.reader = pr
	l.writer = io.Discard
	legMgr.Add(l)
	if err := mgr.AddLeg(roomID, legID); err != nil {
		t.Fatalf("AddLeg: %v", err)
	}
	return l
}

// anchorRoom parks a permanent leg in roomID. The room's mixer is stopped
// whenever the room empties and restarted when it refills, and a restart
// reassigns Mixer.stopCh under a lock readLoop does not take — an unrelated
// pre-existing race the detector trips over. Keeping one leg resident the
// whole test keeps the mixer running so these tests exercise their own
// subject rather than that.
func anchorRoom(t *testing.T, mgr *Manager, legMgr *leg.Manager, roomID string) {
	t.Helper()
	addHealthyLeg(t, mgr, legMgr, roomID, "anchor-"+roomID)
}

// hasLeg reports whether legID is currently a member of r.
func hasLeg(r *Room, legID string) bool {
	for _, l := range r.Participants() {
		if l.ID() == legID {
			return true
		}
	}
	return false
}

// legParticipant returns the mixer instance currently carrying legID's audio.
func legParticipant(t *testing.T, r *Room, legID string) *mixer.Participant {
	t.Helper()
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.legParts[legID]
	if !ok {
		t.Fatalf("no mixer participant recorded for leg %s in room %s", legID, r.ID)
	}
	return p
}

// leftRoomCounter counts leg.left_room events per room.
type leftRoomCounter struct {
	mu sync.Mutex
	n  map[string]int
}

func newLeftRoomCounter(bus *events.Bus) *leftRoomCounter {
	c := &leftRoomCounter{n: map[string]int{}}
	bus.Subscribe(func(e events.Event) {
		if e.Type != events.LegLeftRoom {
			return
		}
		d, ok := e.Data.(*events.LegLeftRoomData)
		if !ok {
			return
		}
		c.mu.Lock()
		c.n[d.RoomID]++
		c.mu.Unlock()
	})
	return c
}

func (c *leftRoomCounter) count(roomID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n[roomID]
}

// TestManager_StalePanicTeardownSparesLegThatLeftAndReturned pins the election
// tearDownPanickedLeg makes.
//
// The teardown runs on a goroutine dispatched by the panicking mixer loop, so
// it can be scheduled arbitrarily late. In that window a leg can leave r1 and
// come back to it, landing on a fresh mixer participant over a live ingress.
// The leg is then a member of r1 again — so an election keyed on room
// membership ("the leg is still here, tear it down") hangs up a healthy call
// and reports cdr.reason=mixer_panic for it. Only the participant instance
// separates the dead path from the replacement.
//
// Invoking tearDownPanickedParticipant directly is what makes the late
// scheduling deterministic. It is the same function wireMixerPanicHook
// dispatches, with the same arguments.
func TestManager_StalePanicTeardownSparesLegThatLeftAndReturned(t *testing.T) {
	bus := newTestBus()
	left := newLeftRoomCounter(bus)

	legMgr := leg.NewManager()
	mgr := NewManager(legMgr, bus, newTestLog())
	var notified int
	mgr.SetOnLegPanicTeardown(func(l leg.Leg, roomID, reason string) { notified++ })

	for _, id := range []string{"r1", "r2"} {
		if _, err := mgr.Create(id, "", 0); err != nil {
			t.Fatalf("Create %s: %v", id, err)
		}
	}
	r1, _ := mgr.Get("r1")
	anchorRoom(t, mgr, legMgr, "r1")
	anchorRoom(t, mgr, legMgr, "r2")

	l := addHealthyLeg(t, mgr, legMgr, "r1", "leg-1")

	// The instance that panics. Everything below happens before the teardown
	// it scheduled gets to run.
	pOld := legParticipant(t, r1, "leg-1")

	if err := mgr.MoveLeg("r1", "r2", "leg-1"); err != nil {
		t.Fatalf("MoveLeg r1->r2: %v", err)
	}
	if err := mgr.MoveLeg("r2", "r1", "leg-1"); err != nil {
		t.Fatalf("MoveLeg r2->r1: %v", err)
	}

	pNew := legParticipant(t, r1, "leg-1")
	if pNew == pOld {
		t.Fatal("returning to r1 did not mint a fresh mixer participant; the test proves nothing")
	}
	if got := left.count("r1"); got != 1 {
		t.Fatalf("leg.left_room for r1 before teardown = %d, want 1 (the move out)", got)
	}

	// The stale teardown finally lands.
	mgr.tearDownPanickedParticipant("r1", pOld, "readLoop")

	if l.hungUp.Load() {
		t.Error("stale teardown hung up a leg that is live in r1 on a fresh participant")
	}
	if got := left.count("r1"); got != 1 {
		t.Errorf("leg.left_room for r1 = %d, want 1; the stale teardown published a duplicate for a leg that never left", got)
	}
	if notified != 0 {
		t.Errorf("owner notified %d times, want 0; a live leg was reported as %s", notified, legPanicReason)
	}
	if !hasLeg(r1, "leg-1") {
		t.Error("the stale teardown evicted the live leg from r1")
	}
	if l.RoomID() != "r1" {
		t.Errorf("leg room = %q, want r1", l.RoomID())
	}
	if legParticipant(t, r1, "leg-1") != pNew {
		t.Error("the live leg's mixer participant was replaced or dropped by the stale teardown")
	}
}

// TestManager_StalePanicTeardownSparesMovedLeg is the same hazard without the
// return trip: the leg is live in r2 when r1's teardown lands. r1 must not
// reach across and hang it up, nor publish a leg.left_room for a room the leg
// has already left.
func TestManager_StalePanicTeardownSparesMovedLeg(t *testing.T) {
	bus := newTestBus()
	left := newLeftRoomCounter(bus)

	legMgr := leg.NewManager()
	mgr := NewManager(legMgr, bus, newTestLog())

	for _, id := range []string{"r1", "r2"} {
		if _, err := mgr.Create(id, "", 0); err != nil {
			t.Fatalf("Create %s: %v", id, err)
		}
	}
	r1, _ := mgr.Get("r1")
	r2, _ := mgr.Get("r2")
	anchorRoom(t, mgr, legMgr, "r1")
	anchorRoom(t, mgr, legMgr, "r2")

	l := addHealthyLeg(t, mgr, legMgr, "r1", "leg-1")
	pOld := legParticipant(t, r1, "leg-1")

	if err := mgr.MoveLeg("r1", "r2", "leg-1"); err != nil {
		t.Fatalf("MoveLeg r1->r2: %v", err)
	}

	mgr.tearDownPanickedParticipant("r1", pOld, "readLoop")

	if l.hungUp.Load() {
		t.Error("stale teardown in r1 hung up a leg that is live in r2")
	}
	if got := left.count("r1"); got != 1 {
		t.Errorf("leg.left_room for r1 = %d, want 1 (the move out only)", got)
	}
	if !hasLeg(r2, "leg-1") {
		t.Error("r1's stale teardown evicted the leg from r2")
	}
}

// TestManager_PanicTeardownStillTearsDownTheLiveInstance is the other side of
// the election: when the participant that panicked is still the leg's audio
// path, teardown must happen in full. An identity check that spared everything
// would pass the tests above while leaving a deaf, mute call connected.
func TestManager_PanicTeardownStillTearsDownTheLiveInstance(t *testing.T) {
	bus := newTestBus()
	left := newLeftRoomCounter(bus)

	legMgr := leg.NewManager()
	mgr := NewManager(legMgr, bus, newTestLog())
	var notified []string
	mgr.SetOnLegPanicTeardown(func(l leg.Leg, roomID, reason string) { notified = append(notified, reason) })

	if _, err := mgr.Create("r1", "", 0); err != nil {
		t.Fatalf("Create: %v", err)
	}
	r1, _ := mgr.Get("r1")
	anchorRoom(t, mgr, legMgr, "r1")

	l := addHealthyLeg(t, mgr, legMgr, "r1", "leg-1")
	p := legParticipant(t, r1, "leg-1")

	mgr.tearDownPanickedParticipant("r1", p, "readLoop")

	if !l.hungUp.Load() {
		t.Error("the panicked leg's live instance was not hung up")
	}
	if got := left.count("r1"); got != 1 {
		t.Errorf("leg.left_room for r1 = %d, want 1", got)
	}
	if hasLeg(r1, "leg-1") {
		t.Error("the panicked leg is still a member of r1")
	}
	if len(notified) != 1 || notified[0] != legPanicReason {
		t.Errorf("owner notifications = %v, want exactly one %q", notified, legPanicReason)
	}
}
