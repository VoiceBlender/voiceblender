//go:build integration

package integration

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gobwas/ws/wsutil"
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
		"tone":   "us_dial",
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
	var statusResp struct {
		Status string `json:"status"`
	}
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
		"tone":   "us_dial",
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
	var statusResp struct {
		Status string `json:"status"`
	}
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

// playbackFinishedFrame is the playback.finished envelope, narrowed to the
// truncation-accounting fields.
type playbackFinishedFrame struct {
	Type       string `json:"type"`
	PlaybackID string `json:"playback_id"`
	Reason     string `json:"reason"`
	PlayedMs   int    `json:"played_ms"`
}

// awaitPlaybackFinished reads VSI frames until the playback.finished for the
// given playback arrives, skipping the other events the call emits.
func awaitPlaybackFinished(t *testing.T, conn net.Conn, playbackID string, within time.Duration) playbackFinishedFrame {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		conn.SetReadDeadline(deadline)
		data, err := wsutil.ReadServerText(conn)
		if err != nil {
			break
		}
		var f playbackFinishedFrame
		if err := json.Unmarshal(data, &f); err != nil {
			continue
		}
		if f.Type == "playback.finished" && f.PlaybackID == playbackID {
			return f
		}
	}
	t.Fatalf("timed out waiting for playback.finished for %s", playbackID)
	return playbackFinishedFrame{}
}

// wavOneSecond8k builds a 1-second 8kHz mono 16-bit silent WAV: 50 ptime frames,
// so a full play must report exactly 1000ms.
func wavOneSecond8k() []byte {
	const sampleRate = 8000
	audio := make([]byte, sampleRate*2) // 1s of 16-bit mono silence

	var buf bytes.Buffer
	buf.WriteString("RIFF")
	binary.Write(&buf, binary.LittleEndian, uint32(36+len(audio)))
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	binary.Write(&buf, binary.LittleEndian, uint32(16))
	binary.Write(&buf, binary.LittleEndian, uint16(1)) // PCM
	binary.Write(&buf, binary.LittleEndian, uint16(1)) // mono
	binary.Write(&buf, binary.LittleEndian, uint32(sampleRate))
	binary.Write(&buf, binary.LittleEndian, uint32(sampleRate*2)) // byte rate
	binary.Write(&buf, binary.LittleEndian, uint16(2))            // block align
	binary.Write(&buf, binary.LittleEndian, uint16(16))           // bits
	buf.WriteString("data")
	binary.Write(&buf, binary.LittleEndian, uint32(len(audio)))
	buf.Write(audio)
	return buf.Bytes()
}

// TestPlaybackFinished_Completed verifies that a prompt played through to its
// end reports reason "completed" and the full duration of the audio.
func TestPlaybackFinished_Completed(t *testing.T) {
	instA := newTestInstance(t, "pbfin-c-a")
	instB := newTestInstance(t, "pbfin-c-b")
	outID, _ := establishCall(t, instA, instB)

	wav := wavOneSecond8k()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/wav")
		w.Write(wav)
	}))
	defer srv.Close()

	conn := dialVSI(t, instA)
	defer conn.Close()

	startResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/play", instA.baseURL(), outID), map[string]interface{}{
		"url":       srv.URL + "/prompt.wav",
		"mime_type": "audio/wav",
	})
	if startResp.StatusCode != http.StatusOK {
		t.Fatalf("play: unexpected status %d", startResp.StatusCode)
	}
	var pbResp struct {
		PlaybackID string `json:"playback_id"`
	}
	decodeJSON(t, startResp, &pbResp)

	fin := awaitPlaybackFinished(t, conn, pbResp.PlaybackID, 15*time.Second)
	if fin.Reason != "completed" {
		t.Errorf("reason = %q, want %q", fin.Reason, "completed")
	}
	// The source is exactly 1s of audio; allow a frame or two of slack.
	if fin.PlayedMs < 950 || fin.PlayedMs > 1050 {
		t.Errorf("played_ms = %d, want ~1000 for a 1s prompt played in full", fin.PlayedMs)
	}
	t.Logf("completed play: reason=%q played_ms=%d", fin.Reason, fin.PlayedMs)
}

// TestPlaybackFinished_Stopped verifies that a prompt cut short reports reason
// "stopped" and only the audio that was actually heard.
func TestPlaybackFinished_Stopped(t *testing.T) {
	instA := newTestInstance(t, "pbfin-s-a")
	instB := newTestInstance(t, "pbfin-s-b")
	outID, _ := establishCall(t, instA, instB)

	conn := dialVSI(t, instA)
	defer conn.Close()

	// A looping tone never ends on its own, so this play can only be stopped.
	startResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/play", instA.baseURL(), outID), map[string]interface{}{
		"tone":   "us_dial",
		"repeat": -1,
	})
	if startResp.StatusCode != http.StatusOK {
		t.Fatalf("play: unexpected status %d", startResp.StatusCode)
	}
	var pbResp struct {
		PlaybackID string `json:"playback_id"`
	}
	decodeJSON(t, startResp, &pbResp)

	// Let a known amount of audio play, then cut it off.
	time.Sleep(500 * time.Millisecond)
	stopResp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s/play/%s", instA.baseURL(), outID, pbResp.PlaybackID))
	if stopResp.StatusCode != http.StatusOK {
		t.Fatalf("stop play: unexpected status %d", stopResp.StatusCode)
	}

	fin := awaitPlaybackFinished(t, conn, pbResp.PlaybackID, 15*time.Second)
	if fin.Reason != "stopped" {
		t.Errorf("reason = %q, want %q", fin.Reason, "stopped")
	}
	if fin.PlayedMs <= 0 {
		t.Errorf("played_ms = %d, want > 0 — audio was playing for ~500ms before the stop", fin.PlayedMs)
	}
	// It played for ~500ms, so it must report far less than the endless tone.
	if fin.PlayedMs > 2000 {
		t.Errorf("played_ms = %d, want well under 2000 for a play stopped after ~500ms", fin.PlayedMs)
	}
	t.Logf("stopped play: reason=%q played_ms=%d", fin.Reason, fin.PlayedMs)
}
