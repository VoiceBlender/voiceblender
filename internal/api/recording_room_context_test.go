package api

import (
	"context"
	"testing"
	"time"
)

// ctxMockLeg is a mock leg whose context the test can cancel, as a hang-up does.
type ctxMockLeg struct {
	*apiMockLeg
	ctx context.Context
}

func (l *ctxMockLeg) Context() context.Context { return l.ctx }

// TestRoomRecording_OutlivesTheLegItStartedWith pins the lifetime of the room
// mix capture to the room recording, not to a participant. A leg's context ends
// when that leg hangs up, and a capture hung on it ends too, while the room and
// its other participants carry on unrecorded.
//
// The room holds one leg when the recording starts, so that leg is the one the
// capture would be hung on.
func TestRoomRecording_OutlivesTheLegItStartedWith(t *testing.T) {
	s := newTestServer(t)
	s.Config.RecordingDir = t.TempDir()

	const roomID = "mix-context-room"
	legCtx, hangUp := context.WithCancel(context.Background())
	defer hangUp()
	first := &ctxMockLeg{apiMockLeg: &apiMockLeg{id: "first", createdAt: time.Now()}, ctx: legCtx}
	s.LegMgr.Add(first)
	if _, err := s.RoomMgr.Create(roomID, "", 0); err != nil {
		t.Fatalf("create room: %v", err)
	}
	if err := s.RoomMgr.AddLeg(roomID, first.ID()); err != nil {
		t.Fatalf("add first leg: %v", err)
	}
	if _, err := s.doStartRecordRoom(context.Background(), roomID, RecordRequest{}); err != nil {
		t.Fatalf("start room recording: %v", err)
	}
	t.Cleanup(func() { s.doDeleteRoom(roomID) })

	second := &apiMockLeg{id: "second", createdAt: time.Now()}
	s.LegMgr.Add(second)
	if err := s.RoomMgr.AddLeg(roomID, second.ID()); err != nil {
		t.Fatalf("add second leg: %v", err)
	}

	roomRecorders.Lock()
	rec := roomRecorders.m[roomID]
	roomRecorders.Unlock()
	if rec == nil || !rec.IsRecording() {
		t.Fatal("precondition: the room recorder is not running")
	}

	hangUp()

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if !rec.IsRecording() {
			t.Fatal("the room mix capture ended when one leg's context did, with another leg still in the room")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
