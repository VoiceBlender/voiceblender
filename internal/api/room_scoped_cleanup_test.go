package api

import (
	"sync"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
)

// startRoomWithRecording creates a room holding one leg and starts a real room
// recording on it through the production path, returning a func reporting
// whether recording.finished has been published for that room.
//
// The recorder is real rather than a seeded map entry, so these tests fail if
// the recording is left running as well as if the map entry is left behind.
func startRoomWithRecording(t *testing.T, s *Server, roomID, legID string) func() bool {
	t.Helper()

	var mu sync.Mutex
	finished := false
	s.Bus.Subscribe(func(e events.Event) {
		if e.Type != events.RecordingFinished {
			return
		}
		d, ok := e.Data.(*events.RecordingFinishedData)
		if !ok || d.RoomID != roomID {
			return
		}
		mu.Lock()
		finished = true
		mu.Unlock()
	})

	s.Config.RecordingDir = t.TempDir()

	l := &apiMockLeg{id: legID, createdAt: time.Now()}
	s.LegMgr.Add(l)
	if _, err := s.RoomMgr.Create(roomID, "", 0); err != nil {
		t.Fatalf("Create room: %v", err)
	}
	if err := s.RoomMgr.AddLeg(roomID, legID); err != nil {
		t.Fatalf("AddLeg: %v", err)
	}
	if _, err := s.doStartRecordRoom(roomID, RecordRequest{}); err != nil {
		t.Fatalf("start room recording: %v", err)
	}

	// Guard against the assertions passing on a recording that never started.
	roomRecorders.Lock()
	_, running := roomRecorders.m[roomID]
	roomRecorders.Unlock()
	if !running {
		t.Fatal("precondition: no room recorder registered after starting one")
	}

	t.Cleanup(func() {
		roomRecorders.Lock()
		delete(roomRecorders.m, roomID)
		roomRecorders.Unlock()
	})

	return func() bool {
		mu.Lock()
		defer mu.Unlock()
		return finished
	}
}

func assertRecordingFinalized(t *testing.T, roomID string, wasFinished func() bool) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for {
		roomRecorders.Lock()
		_, left := roomRecorders.m[roomID]
		roomRecorders.Unlock()
		if !left {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("room recorder still registered: the recording was never " +
				"finalized and nothing reaps it")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !wasFinished() {
		t.Fatal("recording.finished was never published, so a client waiting " +
			"on it waits forever")
	}
}

// TestRemoveLastLegFinalizesRoomRecording covers DELETE /rooms/{id}/legs/{legID}.
//
// This path and a normal last-leg disconnect end in the same state — the room
// still exists with no participants — but only the disconnect used to finalize
// the recording. doRemoveLegFromRoom never called stopRoomRecordingIfEmpty at
// all; it is not a guard that rejected the call, the call was simply absent.
func TestRemoveLastLegFinalizesRoomRecording(t *testing.T) {
	const roomID, legID = "r-remove-last", "leg-remove-last"

	s := newTestServer(t)
	wasFinished := startRoomWithRecording(t, s, roomID, legID)

	if err := s.doRemoveLegFromRoom(roomID, legID); err != nil {
		t.Fatalf("doRemoveLegFromRoom: %v", err)
	}

	assertRecordingFinalized(t, roomID, wasFinished)
}

// TestMoveLastLegOutFinalizesRoomRecording covers moving a room's last leg
// into another room, which leaves the source room alive and empty.
func TestMoveLastLegOutFinalizesRoomRecording(t *testing.T) {
	const fromRoom, toRoom, legID = "r-move-from", "r-move-to", "leg-move"

	s := newTestServer(t)
	wasFinished := startRoomWithRecording(t, s, fromRoom, legID)

	if _, err := s.RoomMgr.Create(toRoom, "", 0); err != nil {
		t.Fatalf("Create destination room: %v", err)
	}
	if _, err := s.doAddLegToRoom(t.Context(), toRoom, AddLegRequest{LegID: legID}); err != nil {
		t.Fatalf("move leg: %v", err)
	}

	assertRecordingFinalized(t, fromRoom, wasFinished)
}

// TestDeleteRoomFinalizesRoomRecording covers DELETE /rooms/{id}.
//
// This one cannot be fixed by routing through roomScopedLegRemoval: deleting
// the room clears every participant's RoomID and drops the room from the
// manager, so both the RoomID guard in cleanupLeg and the Get inside
// stopRoomRecordingIfEmpty fail. doDeleteRoom has to finalize the recording
// explicitly, the way it already does for the room agent.
func TestDeleteRoomFinalizesRoomRecording(t *testing.T) {
	const roomID, legID = "r-delete", "leg-delete"

	s := newTestServer(t)
	wasFinished := startRoomWithRecording(t, s, roomID, legID)

	if err := s.doDeleteRoom(roomID); err != nil {
		t.Fatalf("doDeleteRoom: %v", err)
	}

	assertRecordingFinalized(t, roomID, wasFinished)
}
