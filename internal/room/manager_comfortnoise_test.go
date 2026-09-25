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

func TestManager_CreateWithOptionsOverridesComfortNoise(t *testing.T) {
	off, on := false, true
	cases := []struct {
		name       string
		mgrDefault bool
		override   *bool
		want       bool
	}{
		{"nil keeps default on", true, nil, true},
		{"nil keeps default off", false, nil, false},
		{"false overrides default on", true, &off, false},
		{"true overrides default off", false, &on, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr := NewManager(leg.NewManager(), newTestBus(), newTestLog())
			mgr.SetComfortNoiseEnabled(tc.mgrDefault)
			r, err := mgr.CreateWithOptions("r1", "", mixer.DefaultSampleRate, CreateOptions{ComfortNoise: tc.override})
			if err != nil {
				t.Fatalf("CreateWithOptions: %v", err)
			}
			if got := r.Mixer().ComfortNoiseEnabled(); got != tc.want {
				t.Fatalf("comfort noise = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestManager_ComfortNoiseOverrideDoesNotChangeDefault(t *testing.T) {
	mgr := NewManager(leg.NewManager(), newTestBus(), newTestLog())
	off := false
	if _, err := mgr.CreateWithOptions("r1", "", mixer.DefaultSampleRate, CreateOptions{ComfortNoise: &off}); err != nil {
		t.Fatalf("CreateWithOptions: %v", err)
	}
	r2, err := mgr.Create("r2", "", mixer.DefaultSampleRate)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !r2.Mixer().ComfortNoiseEnabled() {
		t.Fatal("per-room override leaked into the manager default")
	}
}
