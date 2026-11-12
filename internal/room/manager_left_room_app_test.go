package room

import (
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
)

type appMockLeg struct {
	*mockLeg
	appID string
}

func (l *appMockLeg) AppID() string { return l.appID }

// A leg being deleted has left the leg manager before it is taken out of its
// room, so leg.left_room must take the app from the participant.
func TestManager_RemoveLeg_LeftRoomCarriesAppID(t *testing.T) {
	const appID = "room-app"

	for _, tc := range []struct {
		name      string
		inManager bool
	}{
		{"leg still in the manager", true},
		{"leg already out of the manager", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bus := newTestBus()
			legMgr := leg.NewManager()
			mgr := NewManager(legMgr, bus, newTestLog())

			if _, err := mgr.Create("r1", appID, 0); err != nil {
				t.Fatalf("create room: %v", err)
			}
			l := &appMockLeg{mockLeg: newMockLeg("leg-1"), appID: appID}
			legMgr.Add(l)
			if err := mgr.AddLeg("r1", "leg-1"); err != nil {
				t.Fatalf("AddLeg: %v", err)
			}
			if !tc.inManager {
				legMgr.Remove("leg-1")
			}

			var got *events.LegLeftRoomData
			unsub := bus.Subscribe(func(e events.Event) {
				if d, ok := e.Data.(*events.LegLeftRoomData); ok && e.Type == events.LegLeftRoom {
					got = d
				}
			})
			defer unsub()

			if err := mgr.RemoveLeg("r1", "leg-1"); err != nil {
				t.Fatalf("RemoveLeg: %v", err)
			}
			if got == nil {
				t.Fatal("no leg.left_room was published, so this test proves nothing")
			}
			if got.AppID != appID {
				t.Errorf("leg.left_room app_id = %q, want %q", got.AppID, appID)
			}
		})
	}
}
