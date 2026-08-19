package room

import (
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/VoiceBlender/voiceblender/internal/mixer"
)

func TestManager_ComfortNoiseDefaultOn(t *testing.T) {
	mgr := NewManager(leg.NewManager(), newTestBus(), newTestLog())
	r, err := mgr.Create("r1", "", mixer.DefaultSampleRate)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !r.Mixer().ComfortNoiseEnabled() {
		t.Fatal("comfort noise off by default, want on")
	}
}

func TestManager_ComfortNoiseDisabledReachesCreatedRooms(t *testing.T) {
	mgr := NewManager(leg.NewManager(), newTestBus(), newTestLog())
	mgr.SetComfortNoiseEnabled(false)

	r, err := mgr.Create("r1", "", mixer.DefaultSampleRate)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if r.Mixer().ComfortNoiseEnabled() {
		t.Fatal("comfort noise still on after SetComfortNoiseEnabled(false)")
	}
}

func TestManager_ComfortNoiseDisabledReachesMoveCreatedRooms(t *testing.T) {
	legMgr := leg.NewManager()
	mgr := NewManager(legMgr, newTestBus(), newTestLog())
	mgr.SetComfortNoiseEnabled(false)

	if _, err := mgr.Create("r1", "", mixer.DefaultSampleRate); err != nil {
		t.Fatalf("Create: %v", err)
	}
	mover := newMockLeg("mover")
	legMgr.Add(mover)
	if err := mgr.AddLeg("r1", "mover"); err != nil {
		t.Fatalf("AddLeg: %v", err)
	}
	// "r2" does not exist, so MoveLeg is the one that creates it.
	if err := mgr.MoveLeg("r1", "r2", "mover"); err != nil {
		t.Fatalf("MoveLeg: %v", err)
	}

	r2, ok := mgr.Get("r2")
	if !ok {
		t.Fatal("expected r2 to exist after MoveLeg")
	}
	if r2.Mixer().ComfortNoiseEnabled() {
		t.Fatal("move-created room did not pick up the comfort-noise default")
	}
}
