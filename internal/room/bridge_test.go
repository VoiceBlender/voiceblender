package room

import (
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
)

func TestDirection_Valid(t *testing.T) {
	for _, d := range []Direction{DirectionBidirectional, DirectionAToB, DirectionBToA, DirectionNone} {
		if !d.Valid() {
			t.Errorf("%q should be valid", d)
		}
	}
	if Direction("sideways").Valid() {
		t.Error("garbage direction should be invalid")
	}
}

func TestDirection_flags(t *testing.T) {
	cases := []struct {
		dir          Direction
		aSend, bSend bool
	}{
		{DirectionBidirectional, true, true},
		{DirectionAToB, true, false},
		{DirectionBToA, false, true},
		{DirectionNone, false, false},
	}
	for _, c := range cases {
		a, b := c.dir.flags()
		if a != c.aSend || b != c.bSend {
			t.Errorf("%s.flags() = (%v,%v), want (%v,%v)", c.dir, a, b, c.aSend, c.bSend)
		}
	}
}

func newBridgeTestManager(t *testing.T) (*Manager, *events.Bus) {
	t.Helper()
	bus := newTestBus()
	return NewManager(leg.NewManager(), bus, newTestLog()), bus
}

func collectEvents(bus *events.Bus) (*[]events.Event, *sync.Mutex) {
	var mu sync.Mutex
	var got []events.Event
	bus.Subscribe(func(e events.Event) {
		mu.Lock()
		got = append(got, e)
		mu.Unlock()
	})
	return &got, &mu
}

func TestManager_CreateBridge_Success(t *testing.T) {
	mgr, bus := newBridgeTestManager(t)
	got, mu := collectEvents(bus)
	mgr.Create("a", "app1", 16000)
	mgr.Create("b", "", 16000)

	br, err := mgr.CreateBridge("", "a", "b", DirectionBidirectional)
	if err != nil {
		t.Fatalf("CreateBridge: %v", err)
	}
	if br.ID == "" {
		t.Error("expected auto-generated bridge id")
	}

	ra, _ := mgr.Get("a")
	rb, _ := mgr.Get("b")
	// Keepalive: mixers run even with zero legs while a bridge exists.
	if !ra.mixerRunning || !rb.mixerRunning {
		t.Errorf("mixers should be running with a bridge attached (a=%v b=%v)", ra.mixerRunning, rb.mixerRunning)
	}
	if ra.mix.ParticipantCount() != 1 || rb.mix.ParticipantCount() != 1 {
		t.Errorf("each mixer should have the synthetic bridge participant (a=%d b=%d)",
			ra.mix.ParticipantCount(), rb.mix.ParticipantCount())
	}

	mu.Lock()
	defer mu.Unlock()
	var found bool
	for _, e := range *got {
		if e.Type == events.RoomBridged {
			found = true
		}
	}
	if !found {
		t.Error("expected room.bridged event")
	}
}

func TestManager_CreateBridge_Self(t *testing.T) {
	mgr, _ := newBridgeTestManager(t)
	mgr.Create("a", "", 16000)
	_, err := mgr.CreateBridge("", "a", "a", DirectionBidirectional)
	if !errors.Is(err, ErrBridgeSelf) {
		t.Fatalf("err = %v, want ErrBridgeSelf", err)
	}
}

func TestManager_CreateBridge_RoomMissing(t *testing.T) {
	mgr, _ := newBridgeTestManager(t)
	mgr.Create("a", "", 16000)
	if _, err := mgr.CreateBridge("", "a", "ghost", DirectionBidirectional); !errors.Is(err, ErrBridgeRoomMissing) {
		t.Fatalf("err = %v, want ErrBridgeRoomMissing", err)
	}
	if _, err := mgr.CreateBridge("", "ghost", "a", DirectionBidirectional); !errors.Is(err, ErrBridgeRoomMissing) {
		t.Fatalf("err = %v, want ErrBridgeRoomMissing", err)
	}
}

func TestManager_CreateBridge_SampleRateMismatch(t *testing.T) {
	mgr, _ := newBridgeTestManager(t)
	mgr.Create("a", "", 16000)
	mgr.Create("b", "", 8000)
	_, err := mgr.CreateBridge("", "a", "b", DirectionBidirectional)
	if !errors.Is(err, ErrBridgeSampleRate) {
		t.Fatalf("err = %v, want ErrBridgeSampleRate", err)
	}
}

func TestManager_CreateBridge_Duplicate(t *testing.T) {
	mgr, _ := newBridgeTestManager(t)
	mgr.Create("a", "", 16000)
	mgr.Create("b", "", 16000)
	if _, err := mgr.CreateBridge("", "a", "b", DirectionBidirectional); err != nil {
		t.Fatalf("first CreateBridge: %v", err)
	}
	// Reversed pair must also be rejected.
	if _, err := mgr.CreateBridge("", "b", "a", DirectionBidirectional); !errors.Is(err, ErrBridgeExists) {
		t.Fatalf("err = %v, want ErrBridgeExists", err)
	}
}

func TestManager_CreateBridge_InvalidDirection(t *testing.T) {
	mgr, _ := newBridgeTestManager(t)
	mgr.Create("a", "", 16000)
	mgr.Create("b", "", 16000)
	_, err := mgr.CreateBridge("", "a", "b", Direction("loud"))
	if !errors.Is(err, ErrBridgeDirection) {
		t.Fatalf("err = %v, want ErrBridgeDirection", err)
	}
}

func TestManager_SetBridgeDirection(t *testing.T) {
	mgr, bus := newBridgeTestManager(t)
	got, mu := collectEvents(bus)
	mgr.Create("a", "", 16000)
	mgr.Create("b", "", 16000)
	br, _ := mgr.CreateBridge("br1", "a", "b", DirectionBidirectional)

	if err := mgr.SetBridgeDirection("br1", DirectionAToB); err != nil {
		t.Fatalf("SetBridgeDirection: %v", err)
	}
	if got, _ := mgr.GetBridge("br1"); got.Direction != DirectionAToB {
		t.Errorf("direction = %q, want a_to_b", got.Direction)
	}
	if err := mgr.SetBridgeDirection("br1", Direction("nope")); !errors.Is(err, ErrBridgeDirection) {
		t.Errorf("err = %v, want ErrBridgeDirection", err)
	}
	if err := mgr.SetBridgeDirection("missing", DirectionNone); !errors.Is(err, ErrBridgeNotFound) {
		t.Errorf("err = %v, want ErrBridgeNotFound", err)
	}
	_ = br

	mu.Lock()
	defer mu.Unlock()
	var updated int
	for _, e := range *got {
		if e.Type == events.RoomBridgeUpdated {
			updated++
		}
	}
	if updated != 1 {
		t.Errorf("room.bridge_updated count = %d, want 1", updated)
	}
}

func TestManager_DeleteBridge(t *testing.T) {
	mgr, bus := newBridgeTestManager(t)
	got, mu := collectEvents(bus)
	mgr.Create("a", "", 16000)
	mgr.Create("b", "", 16000)
	br, _ := mgr.CreateBridge("br1", "a", "b", DirectionBidirectional)

	if err := mgr.DeleteBridge("br1"); err != nil {
		t.Fatalf("DeleteBridge: %v", err)
	}
	if _, ok := mgr.GetBridge("br1"); ok {
		t.Error("bridge should be gone")
	}
	ra, _ := mgr.Get("a")
	rb, _ := mgr.Get("b")
	if ra.mixerRunning || rb.mixerRunning {
		t.Error("mixers should stop after the only bridge is removed (no legs)")
	}
	if _, err := br.epA.Write([]byte{0, 0}); !errors.Is(err, io.ErrClosedPipe) {
		t.Errorf("conduit endpoint should be closed, Write err = %v", err)
	}
	if err := mgr.DeleteBridge("br1"); !errors.Is(err, ErrBridgeNotFound) {
		t.Errorf("second DeleteBridge err = %v, want ErrBridgeNotFound", err)
	}

	mu.Lock()
	defer mu.Unlock()
	var found bool
	for _, e := range *got {
		if e.Type == events.RoomUnbridged {
			found = true
		}
	}
	if !found {
		t.Error("expected room.unbridged event")
	}
}

func TestManager_DeleteRoom_TearsDownBridges(t *testing.T) {
	mgr, bus := newBridgeTestManager(t)
	got, mu := collectEvents(bus)
	mgr.Create("a", "", 16000)
	mgr.Create("b", "", 16000)
	mgr.CreateBridge("br1", "a", "b", DirectionBidirectional)

	if err := mgr.Delete("b"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := mgr.GetBridge("br1"); ok {
		t.Error("bridge should be torn down when a bridged room is deleted")
	}
	ra, _ := mgr.Get("a")
	if ra.mixerRunning {
		t.Error("surviving room's mixer should stop (no legs, bridge gone)")
	}
	if ra.bridgeRefs != 0 {
		t.Errorf("surviving room bridgeRefs = %d, want 0", ra.bridgeRefs)
	}

	mu.Lock()
	defer mu.Unlock()
	var unbridgedReason string
	var sawUnbridged bool
	for _, e := range *got {
		if e.Type == events.RoomUnbridged {
			sawUnbridged = true
			if d, ok := e.Data.(*events.RoomUnbridgedData); ok {
				unbridgedReason = d.Reason
			}
		}
	}
	if !sawUnbridged {
		t.Fatal("expected room.unbridged event on room delete")
	}
	if unbridgedReason != "room_deleted" {
		t.Errorf("unbridged reason = %q, want room_deleted", unbridgedReason)
	}
}

func TestRoom_KeepaliveLegAndBridge(t *testing.T) {
	mgr, _ := newBridgeTestManager(t)
	mgr.Create("a", "", 16000)
	mgr.Create("b", "", 16000)

	ra, _ := mgr.Get("a")
	l := newMockLeg("leg-1")
	mgr.legMgr.Add(l)
	if err := mgr.AddLeg("a", "leg-1"); err != nil {
		t.Fatalf("AddLeg: %v", err)
	}
	if !ra.mixerRunning {
		t.Fatal("mixer should run with a leg")
	}

	mgr.CreateBridge("br1", "a", "b", DirectionBidirectional)

	// Removing the only leg must NOT stop the mixer while the bridge holds it.
	if err := mgr.RemoveLeg("a", "leg-1"); err != nil {
		t.Fatalf("RemoveLeg: %v", err)
	}
	if !ra.mixerRunning {
		t.Error("mixer must stay alive while a bridge is attached")
	}
	if ra.ParticipantCount() != 0 {
		t.Errorf("room participant (leg) count = %d, want 0", ra.ParticipantCount())
	}

	// Only after the bridge is gone does the mixer stop.
	if err := mgr.DeleteBridge("br1"); err != nil {
		t.Fatalf("DeleteBridge: %v", err)
	}
	if ra.mixerRunning {
		t.Error("mixer should stop once both legs and bridges are gone")
	}
}
