//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// httpPatch sends a PATCH request with a JSON body.
func httpPatch(t *testing.T, url string, body interface{}) *http.Response {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
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

// TestPlaybackVolume_Leg verifies PATCH /v1/legs/{id}/play/{playbackID}.
func TestPlaybackVolume_Leg(t *testing.T) {
	instA := newTestInstance(t, "pbvol-a")
	instB := newTestInstance(t, "pbvol-b")
	outID, _ := establishCall(t, instA, instB)

	// Start a looping tone playback on the outbound leg.
	startResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/play", instA.baseURL(), outID), map[string]interface{}{
		"tone":   "dtmf_0",
		"repeat": -1,
		"volume": 0,
	})
	if startResp.StatusCode != http.StatusOK {
		t.Fatalf("play: unexpected status %d", startResp.StatusCode)
	}
	var pbResp struct {
		PlaybackID string `json:"playback_id"`
	}
	decodeJSON(t, startResp, &pbResp)
	if pbResp.PlaybackID == "" {
		t.Fatal("expected non-empty playback_id")
	}

	// Give the player a moment to start.
	time.Sleep(100 * time.Millisecond)

	// Change volume to +3.
	resp := httpPatch(t, fmt.Sprintf("%s/v1/legs/%s/play/%s", instA.baseURL(), outID, pbResp.PlaybackID), map[string]interface{}{
		"volume": 3,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("volume change: unexpected status %d", resp.StatusCode)
	}
	var statusResp struct{ Status string `json:"status"` }
	decodeJSON(t, resp, &statusResp)
	if statusResp.Status != "ok" {
		t.Fatalf("expected status ok, got %q", statusResp.Status)
	}

	// Change volume to minimum (-8).
	resp = httpPatch(t, fmt.Sprintf("%s/v1/legs/%s/play/%s", instA.baseURL(), outID, pbResp.PlaybackID), map[string]interface{}{
		"volume": -8,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("volume change to -8: unexpected status %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Volume out of range should return 400.
	resp = httpPatch(t, fmt.Sprintf("%s/v1/legs/%s/play/%s", instA.baseURL(), outID, pbResp.PlaybackID), map[string]interface{}{
		"volume": 99,
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for out-of-range volume, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Unknown playback ID should return 404.
	resp = httpPatch(t, fmt.Sprintf("%s/v1/legs/%s/play/%s", instA.baseURL(), outID, "pb-unknown"), map[string]interface{}{
		"volume": 1,
	})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown playback, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Stop the playback.
	stopResp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s/play/%s", instA.baseURL(), outID, pbResp.PlaybackID))
	if stopResp.StatusCode != http.StatusOK {
		t.Fatalf("stop: unexpected status %d", stopResp.StatusCode)
	}
	stopResp.Body.Close()

	// Volume change on a stopped playback should return 404.
	resp = httpPatch(t, fmt.Sprintf("%s/v1/legs/%s/play/%s", instA.baseURL(), outID, pbResp.PlaybackID), map[string]interface{}{
		"volume": 1,
	})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 after stop, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestPlaybackVolume_Room verifies PATCH /v1/rooms/{id}/play/{playbackID}.
func TestPlaybackVolume_Room(t *testing.T) {
	instA := newTestInstance(t, "rmvol-a")
	instB := newTestInstance(t, "rmvol-b")
	outID, _ := establishCall(t, instA, instB)

	// Create a room and add the outbound leg so room has a participant.
	roomID := "vol-test-room"
	createRoomResp := httpPost(t, instA.baseURL()+"/v1/rooms", map[string]interface{}{"id": roomID})
	if createRoomResp.StatusCode != http.StatusCreated {
		t.Fatalf("create room: unexpected status %d", createRoomResp.StatusCode)
	}
	createRoomResp.Body.Close()

	addLegResp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/legs", instA.baseURL(), roomID), map[string]interface{}{
		"leg_id": outID,
	})
	if addLegResp.StatusCode != http.StatusOK {
		t.Fatalf("add leg to room: unexpected status %d", addLegResp.StatusCode)
	}
	addLegResp.Body.Close()

	// Start a looping tone playback on the room.
	startResp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/play", instA.baseURL(), roomID), map[string]interface{}{
		"tone":   "dtmf_0",
		"repeat": -1,
		"volume": 0,
	})
	if startResp.StatusCode != http.StatusOK {
		t.Fatalf("room play: unexpected status %d", startResp.StatusCode)
	}
	var pbResp struct {
		PlaybackID string `json:"playback_id"`
	}
	decodeJSON(t, startResp, &pbResp)
	if pbResp.PlaybackID == "" {
		t.Fatal("expected non-empty playback_id")
	}

	// Give the player a moment to start.
	time.Sleep(100 * time.Millisecond)

	// Change volume to +5.
	resp := httpPatch(t, fmt.Sprintf("%s/v1/rooms/%s/play/%s", instA.baseURL(), roomID, pbResp.PlaybackID), map[string]interface{}{
		"volume": 5,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("room volume change: unexpected status %d", resp.StatusCode)
	}
	var statusResp struct{ Status string `json:"status"` }
	decodeJSON(t, resp, &statusResp)
	if statusResp.Status != "ok" {
		t.Fatalf("expected status ok, got %q", statusResp.Status)
	}

	// Volume out of range should return 400.
	resp = httpPatch(t, fmt.Sprintf("%s/v1/rooms/%s/play/%s", instA.baseURL(), roomID, pbResp.PlaybackID), map[string]interface{}{
		"volume": -99,
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for out-of-range volume, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Unknown playback ID should return 404.
	resp = httpPatch(t, fmt.Sprintf("%s/v1/rooms/%s/play/%s", instA.baseURL(), roomID, "pb-unknown"), map[string]interface{}{
		"volume": 1,
	})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown room playback, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Stop the playback.
	stopResp := httpDelete(t, fmt.Sprintf("%s/v1/rooms/%s/play/%s", instA.baseURL(), roomID, pbResp.PlaybackID))
	if stopResp.StatusCode != http.StatusOK {
		t.Fatalf("room stop: unexpected status %d", stopResp.StatusCode)
	}
	stopResp.Body.Close()
}
