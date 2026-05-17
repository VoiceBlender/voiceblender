//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/room"
)

// constantReader yields an endless stream of a fixed non-silent PCM byte.
type constantReader struct{ b byte }

func (c constantReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = c.b
	}
	return len(p), nil
}

// countingTap counts non-zero PCM bytes written to it.
type countingTap struct {
	mu      sync.Mutex
	nonZero int
}

func (c *countingTap) Write(p []byte) (int, error) {
	c.mu.Lock()
	for _, b := range p {
		if b != 0 {
			c.nonZero++
		}
	}
	c.mu.Unlock()
	return len(p), nil
}

func (c *countingTap) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.nonZero
}

func httpPatchJSON(t *testing.T, url string, body interface{}) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPatch, url, bytes.NewReader(b))
	if err != nil {
		t.Fatalf("new PATCH request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH %s: %v", url, err)
	}
	return resp
}

func createRoom(t *testing.T, inst *testInstance, id string, rate int) {
	t.Helper()
	resp := httpPost(t, inst.baseURL()+"/v1/rooms", map[string]interface{}{"id": id, "sample_rate": rate})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create room %s: status %d", id, resp.StatusCode)
	}
}

// quietRoom returns a room with comfort-noise disabled so that "no audio"
// means an exactly-zero mix (the mixer otherwise fills silence with
// low-level comfort noise, which would mask whether audio actually crossed).
func quietRoom(t *testing.T, inst *testInstance, id string) *room.Room {
	t.Helper()
	r, ok := inst.roomMgr.Get(id)
	if !ok {
		t.Fatalf("room %s not found", id)
	}
	r.Mixer().SetComfortNoise(false)
	return r
}

// waitNonSilence polls until the tap has seen meaningful non-zero audio.
func waitNonSilence(t *testing.T, tap *countingTap, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if tap.count() > 1000 {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func TestRoomBridge_AudioCrossesBidirectional(t *testing.T) {
	inst := newTestInstance(t, "br")
	createRoom(t, inst, "ra", 16000)
	createRoom(t, inst, "rb", 16000)

	resp := httpPost(t, inst.baseURL()+"/v1/rooms/ra/bridges",
		map[string]interface{}{"room_id": "rb", "direction": "bidirectional"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create bridge: status %d", resp.StatusCode)
	}
	var bv struct {
		ID         string `json:"id"`
		RoomID     string `json:"room_id"`
		Direction  string `json:"direction"`
		SampleRate int    `json:"sample_rate"`
	}
	decodeJSON(t, resp, &bv)
	if bv.RoomID != "rb" || bv.Direction != "bidirectional" || bv.SampleRate != 16000 {
		t.Fatalf("unexpected bridge view: %+v", bv)
	}

	inst.collector.waitForMatch(t, events.RoomBridged, func(e events.Event) bool {
		d, ok := e.Data.(*events.RoomBridgedData)
		return ok && d.BridgeID == bv.ID && d.RoomAID == "ra" && d.RoomBID == "rb"
	}, 2*time.Second)

	ra := quietRoom(t, inst, "ra")
	rb := quietRoom(t, inst, "rb")

	// Audio injected into A must reach B's mix.
	tapB := &countingTap{}
	rb.Mixer().SetTap(tapB)
	ra.Mixer().AddPlaybackSource("src-a", constantReader{b: 0x12})
	if !waitNonSilence(t, tapB, 2*time.Second) {
		t.Fatal("room B did not receive audio injected into room A")
	}

	// And the reverse direction.
	tapA := &countingTap{}
	ra.Mixer().SetTap(tapA)
	rb.Mixer().AddPlaybackSource("src-b", constantReader{b: 0x12})
	if !waitNonSilence(t, tapA, 2*time.Second) {
		t.Fatal("room A did not receive audio injected into room B (bidirectional)")
	}
}

func TestRoomBridge_DirectionOneWayAndPatch(t *testing.T) {
	inst := newTestInstance(t, "br")
	createRoom(t, inst, "ra", 16000)
	createRoom(t, inst, "rb", 16000)

	// direction "send" (relative to ra): ra -> rb only.
	resp := httpPost(t, inst.baseURL()+"/v1/rooms/ra/bridges",
		map[string]interface{}{"id": "b1", "room_id": "rb", "direction": "send"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create bridge: status %d", resp.StatusCode)
	}
	resp.Body.Close()

	ra := quietRoom(t, inst, "ra")
	rb := quietRoom(t, inst, "rb")

	// rb -> ra is OFF, so audio injected into rb must NOT reach ra.
	tapA := &countingTap{}
	ra.Mixer().SetTap(tapA)
	rb.Mixer().AddPlaybackSource("src-b", constantReader{b: 0x12})
	time.Sleep(500 * time.Millisecond)
	if tapA.count() != 0 {
		t.Fatalf("room A received audio though rb->ra is off (count=%d)", tapA.count())
	}

	// Flip to "receive" (relative to ra): now rb -> ra is ON.
	presp := httpPatchJSON(t, inst.baseURL()+"/v1/rooms/ra/bridges/b1",
		map[string]interface{}{"direction": "receive"})
	if presp.StatusCode != http.StatusOK {
		t.Fatalf("patch bridge: status %d", presp.StatusCode)
	}
	presp.Body.Close()
	inst.collector.waitForMatch(t, events.RoomBridgeUpdated, func(e events.Event) bool {
		d, ok := e.Data.(*events.RoomBridgeUpdatedData)
		return ok && d.BridgeID == "b1"
	}, 2*time.Second)

	if !waitNonSilence(t, tapA, 2*time.Second) {
		t.Fatal("room A did not receive audio after flipping direction to receive")
	}
}

func TestRoomBridge_None(t *testing.T) {
	inst := newTestInstance(t, "br")
	createRoom(t, inst, "ra", 16000)
	createRoom(t, inst, "rb", 16000)

	resp := httpPost(t, inst.baseURL()+"/v1/rooms/ra/bridges",
		map[string]interface{}{"id": "b1", "room_id": "rb", "direction": "none"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create bridge: status %d", resp.StatusCode)
	}
	resp.Body.Close()

	ra := quietRoom(t, inst, "ra")
	rb := quietRoom(t, inst, "rb")
	tapB := &countingTap{}
	rb.Mixer().SetTap(tapB)
	ra.Mixer().AddPlaybackSource("src-a", constantReader{b: 0x12})
	time.Sleep(500 * time.Millisecond)
	if tapB.count() != 0 {
		t.Fatalf("audio crossed a parked (none) bridge (count=%d)", tapB.count())
	}

	// The bridge is still listed.
	gresp := httpGet(t, inst.baseURL()+"/v1/rooms/ra/bridges")
	var list []map[string]interface{}
	decodeJSON(t, gresp, &list)
	if len(list) != 1 {
		t.Fatalf("expected 1 bridge listed, got %d", len(list))
	}
}

func TestRoomBridge_Validation(t *testing.T) {
	inst := newTestInstance(t, "br")
	createRoom(t, inst, "ra", 16000)
	createRoom(t, inst, "rb", 16000)
	createRoom(t, inst, "rc", 8000)

	cases := []struct {
		name   string
		path   string
		body   map[string]interface{}
		status int
	}{
		{"self", "ra", map[string]interface{}{"room_id": "ra"}, http.StatusBadRequest},
		{"missing-peer", "ra", map[string]interface{}{"room_id": "ghost"}, http.StatusNotFound},
		{"missing-path", "ghost", map[string]interface{}{"room_id": "ra"}, http.StatusNotFound},
		{"rate-mismatch", "ra", map[string]interface{}{"room_id": "rc"}, http.StatusBadRequest},
		{"bad-direction", "ra", map[string]interface{}{"room_id": "rb", "direction": "loud"}, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/bridges", inst.baseURL(), c.path), c.body)
			resp.Body.Close()
			if resp.StatusCode != c.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, c.status)
			}
		})
	}

	// First bridge ok, duplicate (reversed pair) rejected with 409.
	ok := httpPost(t, inst.baseURL()+"/v1/rooms/ra/bridges", map[string]interface{}{"room_id": "rb"})
	if ok.StatusCode != http.StatusCreated {
		t.Fatalf("first bridge status %d", ok.StatusCode)
	}
	ok.Body.Close()
	dup := httpPost(t, inst.baseURL()+"/v1/rooms/rb/bridges", map[string]interface{}{"room_id": "ra"})
	dup.Body.Close()
	if dup.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate bridge status = %d, want 409", dup.StatusCode)
	}
}

func TestRoomBridge_KeepaliveAndRoomDeleteTeardown(t *testing.T) {
	inst := newTestInstance(t, "br")
	createRoom(t, inst, "ra", 16000)
	createRoom(t, inst, "rb", 16000)

	resp := httpPost(t, inst.baseURL()+"/v1/rooms/ra/bridges",
		map[string]interface{}{"id": "b1", "room_id": "rb", "direction": "bidirectional"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create bridge: status %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Both rooms are leg-less; the bridge alone must keep mixers alive so
	// audio injected into the (empty) room A still reaches room B.
	ra := quietRoom(t, inst, "ra")
	rb := quietRoom(t, inst, "rb")
	tapB := &countingTap{}
	rb.Mixer().SetTap(tapB)
	ra.Mixer().AddPlaybackSource("src-a", constantReader{b: 0x12})
	if !waitNonSilence(t, tapB, 2*time.Second) {
		t.Fatal("bridge did not keep the empty room's mixer alive")
	}

	// Deleting room B tears down the bridge and emits room.unbridged.
	del := httpDelete(t, inst.baseURL()+"/v1/rooms/rb")
	del.Body.Close()
	if del.StatusCode != http.StatusOK {
		t.Fatalf("delete room: status %d", del.StatusCode)
	}
	inst.collector.waitForMatch(t, events.RoomUnbridged, func(e events.Event) bool {
		d, ok := e.Data.(*events.RoomUnbridgedData)
		return ok && d.BridgeID == "b1" && d.Reason == "room_deleted"
	}, 2*time.Second)

	gresp := httpGet(t, inst.baseURL()+"/v1/rooms/ra/bridges")
	var list []map[string]interface{}
	decodeJSON(t, gresp, &list)
	if len(list) != 0 {
		t.Fatalf("expected bridge gone after room delete, got %d", len(list))
	}
}
