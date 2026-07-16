package room

import (
	"errors"
	"io"
	"runtime"
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

// TestManager_PanickedLegTeardownGoroutineExits asserts the hook's teardown
// goroutine actually returns. It is the only goroutine the panic path spawns,
// and nothing else in this package observes its exit: mockLeg.Hangup records
// hungUp on entry, so waiting on that proves the goroutine started, not that
// it finished.
func TestManager_PanickedLegTeardownGoroutineExits(t *testing.T) {
	legMgr := leg.NewManager()
	mgr := NewManager(legMgr, newTestBus(), newTestLog())
	if _, err := mgr.Create("r1", "", 0); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Let the room settle before sampling, so the baseline does not include
	// goroutines that are still winding up.
	time.Sleep(50 * time.Millisecond)
	before := runtime.NumGoroutine()

	l := addPanickingLeg(t, mgr, legMgr, "r1", "leg-1")

	r, _ := mgr.Get("r1")
	waitFor(t, 2*time.Second, "panicked leg removed from the room", func() bool {
		return r.ParticipantCount() == 0
	})
	waitFor(t, 2*time.Second, "panicked leg hung up", func() bool {
		return l.hungUp.Load()
	})
	// Generous, because the room's mixLoop is also winding down via
	// syncMixerLocked; a bare assertion here would be flaky.
	waitFor(t, 2*time.Second, "teardown goroutine to exit", func() bool {
		return runtime.NumGoroutine() <= before
	})
}

// TestManager_NonLegParticipantPanicPublishesNothing pins the gate on the
// panic hook's leg assumption. Exactly one of the mixer's production
// registration sites registers a leg; the rest are bridge endpoints and the
// API layer's ws/agent/playback/TTS sources. Treating a panicking playback
// source as a leg would publish leg.left_room — a documented webhook — naming
// an ID that was never a leg and never in the room, while removing and hanging
// up nothing at all.
func TestManager_NonLegParticipantPanicPublishesNothing(t *testing.T) {
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
	r, _ := mgr.Get("r1")

	// A room playback source, not a leg: "pb-1" is unknown to the leg manager.
	r.Mixer().AddPlaybackSource("pb-1", panicReader{})
	r.Mixer().Start()
	defer r.Mixer().Stop()

	waitFor(t, 2*time.Second, "panicked playback source removed from the mixer", func() bool {
		return r.Mixer().ParticipantCount() == 0
	})

	// Let the teardown goroutine run to completion; the publish, if it were
	// coming, happens on that goroutine after the mixer removal above.
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	for _, et := range eventTypes {
		if et == events.LegLeftRoom {
			t.Fatal("leg.left_room published for a playback source that was never a leg")
		}
	}
}

// TestManager_BridgeParticipantPanicTearsDownBridge pins the bridge arm of the
// panic dispatch. Dropping the participant from this room's mixer is only a
// fraction of the job: bridgeRefs is decremented solely by detachBridge, so a
// dead endpoint otherwise keeps mixerShouldRun() true and this room's mixer
// ticking forever, leaves the bridge in the registry, and strands the peer room
// writing into a conduit with no far side.
//
// The panicking reader stands in for the conduit endpoint's Read blowing up:
// registering it under the bridge's participant ID is what makes the mixer's
// real hook fire with a bridge-classed ID.
func TestManager_BridgeParticipantPanicTearsDownBridge(t *testing.T) {
	mgr, bus := newBridgeTestManager(t)
	got, mu := collectEvents(bus)
	mgr.Create("a", "", 16000)
	mgr.Create("b", "", 16000)
	br, err := mgr.CreateBridge("br1", "a", "b", DirectionBidirectional)
	if err != nil {
		t.Fatalf("CreateBridge: %v", err)
	}

	ra, _ := mgr.Get("a")
	rb, _ := mgr.Get("b")
	ra.Mixer().AddPlaybackSource(bridgeParticipantID("br1"), panicReader{})

	waitFor(t, 2*time.Second, "panicked bridge deregistered", func() bool {
		_, ok := mgr.GetBridge("br1")
		return !ok
	})
	waitFor(t, 2*time.Second, "both rooms' mixers to stop after the bridge died", func() bool {
		ra.mu.RLock()
		rb.mu.RLock()
		defer ra.mu.RUnlock()
		defer rb.mu.RUnlock()
		return !ra.mixerRunning && !rb.mixerRunning
	})

	ra.mu.RLock()
	refsA := ra.bridgeRefs
	ra.mu.RUnlock()
	rb.mu.RLock()
	refsB := rb.bridgeRefs
	rb.mu.RUnlock()
	if refsA != 0 || refsB != 0 {
		t.Errorf("bridgeRefs after panic teardown = (a:%d, b:%d), want (0, 0) — the mixers tick forever otherwise", refsA, refsB)
	}

	if _, err := br.epA.Write([]byte{0, 0}); !errors.Is(err, io.ErrClosedPipe) {
		t.Errorf("conduit endpoint should be closed after panic teardown, Write err = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	var sawUnbridged bool
	for _, e := range *got {
		if e.Type == events.RoomUnbridged {
			sawUnbridged = true
		}
		if e.Type == events.LegLeftRoom {
			t.Error("leg.left_room published for a bridge endpoint that was never a leg")
		}
	}
	if !sawUnbridged {
		t.Error("expected room.unbridged when a bridge endpoint's IO panicked")
	}
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
