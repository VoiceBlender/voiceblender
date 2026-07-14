package room

import (
	"io"
	"sync"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
)

// panicReader panics on its first Read, simulating a leg whose inbound audio
// path blows up inside the mixer's readLoop.
type panicReader struct{}

func (panicReader) Read(p []byte) (int, error) { panic("simulated read panic") }

// waitFor polls cond until it is true or the timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", msg)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// addPanickingLeg registers a leg whose mixer readLoop panics on the first
// frame and joins it to roomID.
func addPanickingLeg(t *testing.T, mgr *Manager, legMgr *leg.Manager, roomID, legID string) *mockLeg {
	t.Helper()
	l := newMockLeg(legID)
	l.reader = panicReader{}
	l.writer = io.Discard
	legMgr.Add(l)
	if err := mgr.AddLeg(roomID, legID); err != nil {
		t.Fatalf("AddLeg: %v", err)
	}
	return l
}

// TestManager_MixerParticipantPanicTearsDownLeg verifies the mixer's
// participant-panic hook is wired to real room teardown. Removing the
// participant from the mixer alone would leave a live SIP call connected but
// permanently deaf and mute, with no operator signal — so the leg must also
// leave the room, publish leg.left_room, and be hung up.
func TestManager_MixerParticipantPanicTearsDownLeg(t *testing.T) {
	bus := newTestBus()
	var mu sync.Mutex
	var eventTypes []events.EventType
	bus.Subscribe(func(e events.Event) {
		mu.Lock()
		eventTypes = append(eventTypes, e.Type)
		mu.Unlock()
	})

	legMgr := leg.NewManager()
	mgr := NewManager(legMgr, bus, newTestLog())
	if _, err := mgr.Create("r1", "", 0); err != nil {
		t.Fatalf("Create: %v", err)
	}

	l := addPanickingLeg(t, mgr, legMgr, "r1", "leg-1")

	r, _ := mgr.Get("r1")
	waitFor(t, 2*time.Second, "panicked leg removed from the room", func() bool {
		return r.ParticipantCount() == 0
	})
	waitFor(t, 2*time.Second, "panicked leg hung up", func() bool {
		return l.hungUp.Load()
	})
	waitFor(t, 2*time.Second, "leg.left_room published for the panicked leg", func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, et := range eventTypes {
			if et == events.LegLeftRoom {
				return true
			}
		}
		return false
	})
}

// TestManager_MixerParticipantPanicHangupPanicDoesNotCrash verifies the
// teardown goroutine contains its own panics. It is spawned by the hook, so
// a panic inside Hangup would take the process down — the exact failure this
// teardown exists to prevent.
func TestManager_MixerParticipantPanicHangupPanicDoesNotCrash(t *testing.T) {
	legMgr := leg.NewManager()
	mgr := NewManager(legMgr, newTestBus(), newTestLog())
	if _, err := mgr.Create("r1", "", 0); err != nil {
		t.Fatalf("Create: %v", err)
	}

	l := newMockLeg("leg-1")
	l.reader = panicReader{}
	l.writer = io.Discard
	l.panicOnHangup = true
	legMgr.Add(l)
	if err := mgr.AddLeg("r1", "leg-1"); err != nil {
		t.Fatalf("AddLeg: %v", err)
	}

	r, _ := mgr.Get("r1")
	waitFor(t, 2*time.Second, "panicked leg removed from the room", func() bool {
		return r.ParticipantCount() == 0
	})
	waitFor(t, 2*time.Second, "hangup attempted on the panicked leg", func() bool {
		return l.hungUp.Load()
	})

	// Reaching here at all means the recover held: the panic from Hangup ran
	// on the teardown goroutine, where an escape would have killed the test
	// binary rather than failed this test.
	time.Sleep(100 * time.Millisecond)
}

// TestManager_MoveLegWiresMixerPanicHook guards the second NewRoom site: a
// room created implicitly by MoveLeg must get the same panic teardown as one
// created through Create.
func TestManager_MoveLegWiresMixerPanicHook(t *testing.T) {
	legMgr := leg.NewManager()
	mgr := NewManager(legMgr, newTestBus(), newTestLog())
	if _, err := mgr.Create("r1", "", 0); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A non-panicking leg to move, so "r2" is created by the move path.
	mover := newMockLeg("mover")
	legMgr.Add(mover)
	if err := mgr.AddLeg("r1", "mover"); err != nil {
		t.Fatalf("AddLeg: %v", err)
	}
	if err := mgr.MoveLeg("r1", "r2", "mover"); err != nil {
		t.Fatalf("MoveLeg: %v", err)
	}

	r2, ok := mgr.Get("r2")
	if !ok {
		t.Fatal("expected r2 to exist after MoveLeg")
	}

	l := addPanickingLeg(t, mgr, legMgr, "r2", "leg-1")

	waitFor(t, 2*time.Second, "panicked leg torn down in the move-created room", func() bool {
		return l.hungUp.Load()
	})
	if r2.ParticipantCount() != 1 {
		t.Errorf("r2 count = %d, want 1 (only the mover left)", r2.ParticipantCount())
	}
}

// TestManager_DeletePanickingLegDoesNotCrash verifies that a panic inside
// one leg's Hangup during the room-delete fan-out is recovered rather than
// crashing the process: Delete still returns (wg completes), the room is
// removed from the map, and RoomDeleted still publishes.
func TestManager_DeletePanickingLegDoesNotCrash(t *testing.T) {
	bus := newTestBus()
	var eventTypes []events.EventType
	bus.Subscribe(func(e events.Event) {
		eventTypes = append(eventTypes, e.Type)
	})

	legMgr := leg.NewManager()
	mgr := NewManager(legMgr, bus, newTestLog())

	r, err := mgr.Create("r1", "", 0)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	good := newMockLeg("good")
	bad := newMockLeg("bad")
	bad.panicOnHangup = true
	r.AddLeg(good)
	r.AddLeg(bad)

	if err := mgr.Delete("r1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, ok := mgr.Get("r1"); ok {
		t.Error("room should be deleted")
	}

	found := false
	for _, et := range eventTypes {
		if et == events.RoomDeleted {
			found = true
		}
	}
	if !found {
		t.Error("expected room.deleted event after a panicking leg hangup")
	}
}
