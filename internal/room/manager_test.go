package room

import (
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"
	"sync/atomic"
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

// TestManager_CreateDoesNotHoldLockWhilePublishing pins the fix for a live
// self-deadlock, not a style point. Bus.Publish runs handlers synchronously on
// the caller's goroutine and sync.RWMutex is not reentrant, so publishing
// room.created under m.mu wedges any subscriber that calls back into the
// manager. It also asserts the room is discoverable by the time the event
// announces it, which is the ordering the old locked publish gave for free.
func TestManager_CreateDoesNotHoldLockWhilePublishing(t *testing.T) {
	bus := newTestBus()
	mgr := NewManager(leg.NewManager(), bus, newTestLog())

	var probeReturned, found atomic.Bool
	bus.Subscribe(func(e events.Event) {
		if e.Type != events.RoomCreated {
			return
		}
		// Probe from another goroutine and block this one until it answers.
		// Calling Get inline would deadlock Create's own goroutine forever
		// instead of failing; and returning without waiting would release
		// m.mu before the probe ran, so the probe must finish inside the
		// publish window to prove anything.
		probeDone := make(chan struct{})
		go func() {
			defer close(probeDone)
			_, ok := mgr.Get("r1")
			found.Store(ok)
		}()
		select {
		case <-probeDone:
			probeReturned.Store(true)
		case <-time.After(2 * time.Second):
		}
	})

	if _, err := mgr.Create("r1", "", 0); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if !probeReturned.Load() {
		t.Fatal("room.created published while m.mu was held: subscriber's Get() blocked")
	}
	if !found.Load() {
		t.Fatal("room.created published before the room was discoverable")
	}
}

// TestManager_MoveLegDoesNotHoldLockWhilePublishing is the Create test's twin
// for MoveLeg's fallback room creation. That branch is only reachable when the
// target room is absent — the sole API caller creates it first, so in
// production it takes a delete race — but the violation is identical and the
// unit API reaches it directly.
func TestManager_MoveLegDoesNotHoldLockWhilePublishing(t *testing.T) {
	bus := newTestBus()
	legMgr := leg.NewManager()
	mgr := NewManager(legMgr, bus, newTestLog())
	if _, err := mgr.Create("from", "", 0); err != nil {
		t.Fatalf("Create: %v", err)
	}
	mover := newMockLeg("mover")
	legMgr.Add(mover)
	if err := mgr.AddLeg("from", "mover"); err != nil {
		t.Fatalf("AddLeg: %v", err)
	}

	// Subscribed after the source room exists, so the only room.created seen
	// here is the one MoveLeg publishes for the target.
	var probeReturned, found atomic.Bool
	bus.Subscribe(func(e events.Event) {
		if e.Type != events.RoomCreated {
			return
		}
		probeDone := make(chan struct{})
		go func() {
			defer close(probeDone)
			_, ok := mgr.Get("to")
			found.Store(ok)
		}()
		select {
		case <-probeDone:
			probeReturned.Store(true)
		case <-time.After(2 * time.Second):
		}
	})

	if err := mgr.MoveLeg("from", "to", "mover"); err != nil {
		t.Fatalf("MoveLeg: %v", err)
	}

	if !probeReturned.Load() {
		t.Fatal("MoveLeg published room.created while m.mu was held: subscriber's Get() blocked")
	}
	if !found.Load() {
		t.Fatal("MoveLeg published room.created before the target room was discoverable")
	}
}

// TestManager_MoveLegConcurrentCreatePublishesOnce pins the double-check that
// replaced the single locked !ok branch. room.created is a public webhook, so
// concurrent moves into the same absent room must announce it exactly once —
// the guarantee the old lock-around-everything gave, which the fix has to keep
// by other means.
//
// The get-or-create window this races on is only a few hundred nanoseconds
// wide (NewRoom plus the Lock), so one round catches a regression maybe one
// time in five on a loaded machine. Rounds are repeated to make the guard
// dependable rather than decorative: an unguarded insert is caught on every
// observed run.
func TestManager_MoveLegConcurrentCreatePublishesOnce(t *testing.T) {
	const (
		rounds = 150
		movers = 4
	)

	bus := newTestBus()
	legMgr := leg.NewManager()
	mgr := NewManager(legMgr, bus, newTestLog())

	srcRoom := func(r, i int) string { return fmt.Sprintf("from-%d-%d", r, i) }
	legID := func(r, i int) string { return fmt.Sprintf("leg-%d-%d", r, i) }
	target := func(r int) string { return fmt.Sprintf("to-%d", r) }

	for r := 0; r < rounds; r++ {
		for i := 0; i < movers; i++ {
			if _, err := mgr.Create(srcRoom(r, i), "", 0); err != nil {
				t.Fatalf("Create %s: %v", srcRoom(r, i), err)
			}
			l := newMockLeg(legID(r, i))
			legMgr.Add(l)
			if err := mgr.AddLeg(srcRoom(r, i), l.ID()); err != nil {
				t.Fatalf("AddLeg: %v", err)
			}
		}
	}

	// Subscribed after the fixture, so only the targets' room.created is seen.
	var mu sync.Mutex
	created := map[string]int{}
	bus.Subscribe(func(e events.Event) {
		d, ok := e.Data.(*events.RoomCreatedData)
		if !ok || e.Type != events.RoomCreated {
			return
		}
		mu.Lock()
		created[d.RoomID]++
		mu.Unlock()
	})

	for r := 0; r < rounds; r++ {
		// Spin barrier, not a channel close: waking N goroutines off a closed
		// channel lets the runtime release them one at a time, and the first
		// can finish its whole move before the last wakes, so the window never
		// overlaps. Spinning keeps every mover hot on its own P.
		var ready sync.WaitGroup
		var gate atomic.Bool
		var wg sync.WaitGroup
		ready.Add(movers)
		for i := 0; i < movers; i++ {
			wg.Add(1)
			go func(r, i int) {
				defer wg.Done()
				ready.Done()
				for !gate.Load() {
					runtime.Gosched()
				}
				if err := mgr.MoveLeg(srcRoom(r, i), target(r), legID(r, i)); err != nil {
					t.Errorf("MoveLeg: %v", err)
				}
			}(r, i)
		}
		ready.Wait()
		gate.Store(true)
		wg.Wait()
	}

	mu.Lock()
	defer mu.Unlock()
	for r := 0; r < rounds; r++ {
		if n := created[target(r)]; n != 1 {
			t.Fatalf("room.created for %s published %d times, want exactly 1", target(r), n)
		}
	}
}

// TestManager_PanickedLegNotifiesOwner drives the real path — a leg whose
// AudioReader panics, through the real mixer readLoop, the real
// SetOnParticipantPanic hook, into tearDownPanickedLeg — and asserts the owner
// is told. Without the notification the room layer hangs the leg up and the
// API layer never learns, so no CDR is published and the leg leaks in the leg
// manager.
func TestManager_PanickedLegNotifiesOwner(t *testing.T) {
	legMgr := leg.NewManager()
	mgr := NewManager(legMgr, newTestBus(), newTestLog())

	type notice struct {
		leg    leg.Leg
		roomID string
		reason string
	}
	var mu sync.Mutex
	var notices []notice
	mgr.SetOnLegPanicTeardown(func(l leg.Leg, roomID, reason string) {
		mu.Lock()
		notices = append(notices, notice{leg: l, roomID: roomID, reason: reason})
		mu.Unlock()
	})

	if _, err := mgr.Create("r1", "", 0); err != nil {
		t.Fatalf("Create: %v", err)
	}
	l := addPanickingLeg(t, mgr, legMgr, "r1", "leg-1")

	waitFor(t, 2*time.Second, "owner notified of the panicked leg", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(notices) > 0
	})

	mu.Lock()
	defer mu.Unlock()
	if len(notices) != 1 {
		t.Fatalf("owner notified %d times, want exactly 1", len(notices))
	}
	if notices[0].leg.ID() != "leg-1" {
		t.Fatalf("owner notified for leg %q, want %q", notices[0].leg.ID(), "leg-1")
	}
	if notices[0].reason != "mixer_panic" {
		t.Fatalf("reason = %q, want %q", notices[0].reason, "mixer_panic")
	}
	// The owner's callback runs cleanupLeg, which hangs the leg up; the room
	// must have done that already rather than leaving it to a hook that may
	// not be installed.
	if !l.hungUp.Load() {
		t.Fatal("owner notified before the panicked leg was hung up")
	}
}
