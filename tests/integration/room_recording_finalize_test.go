//go:build integration

package integration

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
)

// A room recording is finalized when the room is left with no participants,
// whichever path emptied it. Every path below ends in the same state a normal
// last-leg disconnect does, and each used to be reached by its own hand-written
// copy of the room-scoped cleanup — two of which omitted the recording, leaving
// the recorder writing to an open file that nothing reaps and a client waiting
// on recording.finished forever.

// startRoomRecordingWithCall establishes a call, puts the outbound leg in a
// fresh room with the given app ID and starts a room recording on it. It
// returns the room ID, the leg ID and the file being written.
func startRoomRecordingWithCall(t *testing.T, instA, instB *testInstance, appID string) (roomID, legID, file string) {
	t.Helper()

	legID, _ = establishCall(t, instA, instB)

	roomResp := httpPost(t, instA.baseURL()+"/v1/rooms", map[string]interface{}{
		"app_id":      appID,
		"sample_rate": 16000,
	})
	if roomResp.StatusCode != http.StatusCreated {
		t.Fatalf("create room: status %d", roomResp.StatusCode)
	}
	var rm roomView
	decodeJSON(t, roomResp, &rm)

	addResp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/legs", instA.baseURL(), rm.ID),
		map[string]interface{}{"leg_id": legID})
	if addResp.StatusCode != http.StatusOK {
		t.Fatalf("add leg to room: status %d", addResp.StatusCode)
	}
	addResp.Body.Close()
	instA.collector.waitForMatch(t, events.LegJoinedRoom, func(e events.Event) bool {
		return e.Data.GetLegID() == legID && e.Data.GetRoomID() == rm.ID
	}, 3*time.Second)

	recResp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/record", instA.baseURL(), rm.ID), map[string]interface{}{})
	if recResp.StatusCode != http.StatusOK {
		t.Fatalf("start room recording: status %d", recResp.StatusCode)
	}
	var recStart recordingResponse
	decodeJSON(t, recResp, &recStart)
	if recStart.Status != "recording" || recStart.File == "" {
		t.Fatalf("start room recording: got %+v", recStart)
	}
	instA.collector.waitForMatch(t, events.RecordingStarted, func(e events.Event) bool {
		return e.Data.GetRoomID() == rm.ID
	}, 3*time.Second)

	// Give the mixer time to push real audio through the tap.
	time.Sleep(500 * time.Millisecond)

	return rm.ID, legID, recStart.File
}

// assertRoomRecordingFinalized waits for recording.finished on the room and
// checks that the recorder is really gone rather than just unreported: a
// second stop must 404, and the file must stop growing.
func assertRoomRecordingFinalized(t *testing.T, inst *testInstance, roomID, appID, wantFile string) {
	t.Helper()

	e := inst.collector.waitForMatch(t, events.RecordingFinished, func(e events.Event) bool {
		return e.Data.GetRoomID() == roomID
	}, 5*time.Second)
	d, ok := e.Data.(*events.RecordingFinishedData)
	if !ok {
		t.Fatalf("recording.finished data type = %T", e.Data)
	}
	if d.File != wantFile {
		t.Errorf("recording.finished file = %q, want %q", d.File, wantFile)
	}
	if d.AppID != appID {
		t.Errorf("recording.finished app_id = %q, want %q", d.AppID, appID)
	}

	stopResp := httpDelete(t, fmt.Sprintf("%s/v1/rooms/%s/record", inst.baseURL(), roomID))
	stopResp.Body.Close()
	if stopResp.StatusCode != http.StatusNotFound {
		t.Errorf("stop after finalize: status %d, want 404 — the recorder is still registered", stopResp.StatusCode)
	}

	before := fileSize(t, wantFile)
	time.Sleep(300 * time.Millisecond)
	if after := fileSize(t, wantFile); after != before {
		t.Errorf("recording file still growing: %d -> %d bytes", before, after)
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat recording file: %v", err)
	}
	return fi.Size()
}

// TestRoomRecordingFinalize_RemoveLastLeg covers DELETE /v1/rooms/{id}/legs/{legID}.
func TestRoomRecordingFinalize_RemoveLastLeg(t *testing.T) {
	instA := newTestInstance(t, "recfin-remove-a")
	instB := newTestInstance(t, "recfin-remove-b")

	roomID, legID, file := startRoomRecordingWithCall(t, instA, instB, "app-remove")

	removeResp := httpDelete(t, fmt.Sprintf("%s/v1/rooms/%s/legs/%s", instA.baseURL(), roomID, legID))
	if removeResp.StatusCode != http.StatusOK {
		t.Fatalf("remove leg from room: status %d", removeResp.StatusCode)
	}
	removeResp.Body.Close()

	assertRoomRecordingFinalized(t, instA, roomID, "app-remove", file)
	assertWAVAudio(t, file, 1, 16000, 100)

	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), legID)).Body.Close()
}

// TestRoomRecordingFinalize_MoveLastLegOut covers moving a room's last leg into
// another room, which leaves the source room alive and empty.
func TestRoomRecordingFinalize_MoveLastLegOut(t *testing.T) {
	instA := newTestInstance(t, "recfin-move-a")
	instB := newTestInstance(t, "recfin-move-b")

	fromRoom, legID, file := startRoomRecordingWithCall(t, instA, instB, "app-move")

	toResp := httpPost(t, instA.baseURL()+"/v1/rooms", map[string]interface{}{"sample_rate": 16000})
	var toRoom roomView
	decodeJSON(t, toResp, &toRoom)

	moveResp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/legs", instA.baseURL(), toRoom.ID),
		map[string]interface{}{"leg_id": legID})
	if moveResp.StatusCode != http.StatusOK {
		t.Fatalf("move leg: status %d", moveResp.StatusCode)
	}
	moveResp.Body.Close()

	assertRoomRecordingFinalized(t, instA, fromRoom, "app-move", file)
	assertWAVAudio(t, file, 1, 16000, 100)

	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), legID)).Body.Close()
}

// TestRoomRecordingFinalize_DeleteRoom covers DELETE /v1/rooms/{id}. This path
// cannot go through the shared room-scoped cleanup: deleting the room clears
// every participant's RoomID and drops the room from the manager, so the
// per-leg cleanup skips its room block and the empty-room lookup fails too.
// doDeleteRoom finalizes the recording explicitly, the way it already does for
// the room agent, which is also why the app ID has to be snapshotted before
// the room goes away.
func TestRoomRecordingFinalize_DeleteRoom(t *testing.T) {
	instA := newTestInstance(t, "recfin-delete-a")
	instB := newTestInstance(t, "recfin-delete-b")

	roomID, legID, file := startRoomRecordingWithCall(t, instA, instB, "app-delete")

	delResp := httpDelete(t, fmt.Sprintf("%s/v1/rooms/%s", instA.baseURL(), roomID))
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("delete room: status %d", delResp.StatusCode)
	}
	delResp.Body.Close()

	assertRoomRecordingFinalized(t, instA, roomID, "app-delete", file)
	assertWAVAudio(t, file, 1, 16000, 100)

	instA.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == legID
	}, 5*time.Second)
}

// TestRoomRecordingFinalize_MixerPanicOnLastLeg covers the teardown the room
// layer drives when a participant's audio path panics. That leg is already out
// of the room by the time the API layer sees it, so the teardown has to run the
// room-scoped cleanup itself — and it is the fourth site that has to remember
// the recording.
//
// The panicked leg is armed only after the live call has left, so it is the leg
// whose removal empties the room.
func TestRoomRecordingFinalize_MixerPanicOnLastLeg(t *testing.T) {
	instA := newTestInstance(t, "recfin-panic-a")
	instB := newTestInstance(t, "recfin-panic-b")

	roomID, legID, file := startRoomRecordingWithCall(t, instA, instB, "app-panic")

	bad := newDisarmedPanicLeg("recfin-panic-leg")
	t.Cleanup(func() { bad.Hangup(context.Background()) })
	instA.legMgr.Add(bad)
	if err := instA.roomMgr.AddLeg(roomID, bad.ID()); err != nil {
		t.Fatalf("add panic leg to room: %v", err)
	}

	// Take the call out first. The room still holds the panic leg, so the
	// recording must NOT be finalized yet.
	removeResp := httpDelete(t, fmt.Sprintf("%s/v1/rooms/%s/legs/%s", instA.baseURL(), roomID, legID))
	removeResp.Body.Close()
	instA.collector.waitForMatch(t, events.LegLeftRoom, func(e events.Event) bool {
		return e.Data.GetLegID() == legID
	}, 3*time.Second)
	if instA.collector.hasEvent(events.RecordingFinished, func(e events.Event) bool {
		return e.Data.GetRoomID() == roomID
	}) {
		t.Fatal("recording finalized while the room still had a participant")
	}

	bad.arm()
	select {
	case <-bad.writer.fired:
	case <-time.After(5 * time.Second):
		t.Fatal("panic leg was never written to; the mixer is not running")
	}

	assertRoomRecordingFinalized(t, instA, roomID, "app-panic", file)
	assertWAVAudio(t, file, 1, 16000, 100)

	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), legID)).Body.Close()
}
