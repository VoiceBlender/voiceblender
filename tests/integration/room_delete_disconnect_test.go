//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
)

// TestRoomDelete_DisconnectsLegsWithReason verifies that DELETE /v1/rooms/{id}
// publishes leg.disconnected with reason "room_deleted" for every leg that
// was in the room — this used to silently drop the disconnect event.
func TestRoomDelete_DisconnectsLegsWithReason(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	outboundID, _ := establishCall(t, instA, instB)

	roomResp := httpPost(t, instA.baseURL()+"/v1/rooms", map[string]interface{}{})
	var rm roomView
	decodeJSON(t, roomResp, &rm)

	addResp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/legs", instA.baseURL(), rm.ID),
		map[string]interface{}{"leg_id": outboundID})
	addResp.Body.Close()
	instA.collector.waitForMatch(t, events.LegJoinedRoom, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundID
	}, 3*time.Second)

	// Delete the room; the leg in it must disconnect with reason="room_deleted".
	delResp := httpDelete(t, fmt.Sprintf("%s/v1/rooms/%s", instA.baseURL(), rm.ID))
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("delete room: status %d", delResp.StatusCode)
	}
	delResp.Body.Close()

	disc := instA.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundID
	}, 5*time.Second)
	if got := disc.Data.(*events.LegDisconnectedData).CDR.Reason; got != "room_deleted" {
		t.Errorf("cdr.reason = %q, want room_deleted", got)
	}

	// And the leg must be gone from the manager (404 on subsequent GET).
	time.Sleep(100 * time.Millisecond)
	getResp := httpGet(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
	getResp.Body.Close()
	if getResp.StatusCode != http.StatusNotFound {
		t.Errorf("GET leg after room delete: status %d, want 404", getResp.StatusCode)
	}
}

// TestRoomDelete_RaceWithLegDelete verifies that a concurrent
// DELETE /v1/rooms/{id} and DELETE /v1/legs/{id} on the only participant
// produces exactly one leg.disconnected event (the dedup gate handles the
// race between the two cleanup paths).
func TestRoomDelete_RaceWithLegDelete(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	outboundID, _ := establishCall(t, instA, instB)

	roomResp := httpPost(t, instA.baseURL()+"/v1/rooms", map[string]interface{}{})
	var rm roomView
	decodeJSON(t, roomResp, &rm)

	addResp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/legs", instA.baseURL(), rm.ID),
		map[string]interface{}{"leg_id": outboundID})
	addResp.Body.Close()
	instA.collector.waitForMatch(t, events.LegJoinedRoom, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundID
	}, 3*time.Second)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		r := httpDelete(t, fmt.Sprintf("%s/v1/rooms/%s", instA.baseURL(), rm.ID))
		r.Body.Close()
	}()
	go func() {
		defer wg.Done()
		r := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
		r.Body.Close()
	}()
	wg.Wait()

	instA.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundID
	}, 5*time.Second)
	time.Sleep(300 * time.Millisecond) // settle window for any racing duplicate

	if got := len(instA.collector.matchAll(events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundID
	})); got != 1 {
		t.Errorf("leg.disconnected count = %d, want 1 (dedup must prevent duplicate from racing room/leg DELETEs)", got)
	}
}
