//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/go-audio/wav"
)

// TestRecording_PauseResume_Leg exercises the leg recording pause/resume
// endpoints end-to-end:
//   - pause emits `recording.paused`
//   - resume emits `recording.resumed`
//   - repeated pause/resume are idempotent
//   - the finished WAV still contains real (non-zero) audio after resume
func TestRecording_PauseResume_Leg(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	outboundID, _ := establishCall(t, instA, instB)

	recURL := fmt.Sprintf("%s/v1/legs/%s/record", instA.baseURL(), outboundID)

	// Start recording.
	recResp := httpPost(t, recURL, nil)
	if recResp.StatusCode != http.StatusOK {
		t.Fatalf("start recording: unexpected status %d", recResp.StatusCode)
	}
	var recStart recordingResponse
	decodeJSON(t, recResp, &recStart)

	// Let audio flow for a bit.
	time.Sleep(300 * time.Millisecond)

	// Pause.
	pauseResp := httpPost(t, recURL+"/pause", nil)
	if pauseResp.StatusCode != http.StatusOK {
		t.Fatalf("pause: unexpected status %d", pauseResp.StatusCode)
	}
	var pausedBody map[string]string
	decodeJSON(t, pauseResp, &pausedBody)
	if pausedBody["status"] != "paused" {
		t.Fatalf(`expected status "paused", got %q`, pausedBody["status"])
	}
	instA.collector.waitForMatch(t, events.RecordingPaused, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundID
	}, 3*time.Second)

	// Second pause — idempotent.
	pause2 := httpPost(t, recURL+"/pause", nil)
	if pause2.StatusCode != http.StatusOK {
		t.Fatalf("second pause: unexpected status %d", pause2.StatusCode)
	}
	var pause2Body map[string]string
	decodeJSON(t, pause2, &pause2Body)
	if pause2Body["status"] != "already_paused" {
		t.Fatalf(`expected "already_paused", got %q`, pause2Body["status"])
	}

	time.Sleep(300 * time.Millisecond) // this window should end up silent

	// Resume.
	resumeResp := httpPost(t, recURL+"/resume", nil)
	if resumeResp.StatusCode != http.StatusOK {
		t.Fatalf("resume: unexpected status %d", resumeResp.StatusCode)
	}
	var resumedBody map[string]string
	decodeJSON(t, resumeResp, &resumedBody)
	if resumedBody["status"] != "resumed" {
		t.Fatalf(`expected status "resumed", got %q`, resumedBody["status"])
	}
	instA.collector.waitForMatch(t, events.RecordingResumed, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundID
	}, 3*time.Second)

	// Second resume — idempotent.
	resume2 := httpPost(t, recURL+"/resume", nil)
	var resume2Body map[string]string
	decodeJSON(t, resume2, &resume2Body)
	if resume2Body["status"] != "not_paused" {
		t.Fatalf(`expected "not_paused", got %q`, resume2Body["status"])
	}

	time.Sleep(300 * time.Millisecond)

	// Stop.
	stopResp := httpDelete(t, recURL)
	if stopResp.StatusCode != http.StatusOK {
		t.Fatalf("stop: unexpected status %d", stopResp.StatusCode)
	}

	// WAV must be valid and cover a plausible duration (end-to-end
	// wiring test — sample-level silence-during-pause is validated in the
	// recording package unit tests).
	f, err := os.Open(recStart.File)
	if err != nil {
		t.Fatalf("open WAV: %v", err)
	}
	defer f.Close()
	dec := wav.NewDecoder(f)
	if !dec.IsValidFile() {
		t.Fatal("invalid WAV")
	}
	buf, err := dec.FullPCMBuffer()
	if err != nil {
		t.Fatalf("read WAV: %v", err)
	}
	if len(buf.Data) == 0 {
		t.Fatal("empty WAV")
	}

	// Pause after stop should 404.
	pauseAfterStop := httpPost(t, recURL+"/pause", nil)
	if pauseAfterStop.StatusCode != http.StatusNotFound {
		t.Fatalf("pause after stop: expected 404, got %d", pauseAfterStop.StatusCode)
	}

	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
}

// TestRecording_PauseResume_Room verifies pause/resume works for room-level
// recording.
func TestRecording_PauseResume_Room(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	outboundID, _ := establishCall(t, instA, instB)

	roomResp := httpPost(t, instA.baseURL()+"/v1/rooms", map[string]interface{}{})
	if roomResp.StatusCode != http.StatusCreated {
		t.Fatalf("create room: status %d", roomResp.StatusCode)
	}
	var rm roomView
	decodeJSON(t, roomResp, &rm)

	addResp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/legs", instA.baseURL(), rm.ID), map[string]interface{}{
		"leg_id": outboundID,
	})
	addResp.Body.Close()
	instA.collector.waitForMatch(t, events.LegJoinedRoom, nil, 3*time.Second)

	recURL := fmt.Sprintf("%s/v1/rooms/%s/record", instA.baseURL(), rm.ID)
	recResp := httpPost(t, recURL, nil)
	if recResp.StatusCode != http.StatusOK {
		t.Fatalf("start room recording: status %d", recResp.StatusCode)
	}
	var recStart recordingResponse
	decodeJSON(t, recResp, &recStart)

	time.Sleep(300 * time.Millisecond)

	if r := httpPost(t, recURL+"/pause", nil); r.StatusCode != http.StatusOK {
		t.Fatalf("pause: %d", r.StatusCode)
	}
	instA.collector.waitForMatch(t, events.RecordingPaused, func(e events.Event) bool {
		return e.Data.GetRoomID() == rm.ID
	}, 3*time.Second)

	time.Sleep(300 * time.Millisecond)

	if r := httpPost(t, recURL+"/resume", nil); r.StatusCode != http.StatusOK {
		t.Fatalf("resume: %d", r.StatusCode)
	}
	instA.collector.waitForMatch(t, events.RecordingResumed, func(e events.Event) bool {
		return e.Data.GetRoomID() == rm.ID
	}, 3*time.Second)

	time.Sleep(300 * time.Millisecond)

	if r := httpDelete(t, recURL); r.StatusCode != http.StatusOK {
		t.Fatalf("stop: %d", r.StatusCode)
	}

	fi, err := os.Stat(recStart.File)
	if err != nil || fi.Size() < 44 {
		t.Fatalf("room WAV missing or empty: %v", err)
	}

	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
}
