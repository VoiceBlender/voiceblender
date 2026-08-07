//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
)

func TestRecording_CustomFilename(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	outboundID, _ := establishCall(t, instA, instB)

	name := "call.v2-" + outboundID[:8]
	recResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/record", instA.baseURL(), outboundID), map[string]interface{}{
		"filename": name,
	})
	if recResp.StatusCode != http.StatusOK {
		t.Fatalf("start recording: unexpected status %d", recResp.StatusCode)
	}
	var recStart recordingResponse
	decodeJSON(t, recResp, &recStart)
	wantBase := name + ".wav"
	if filepath.Base(recStart.File) != wantBase {
		t.Fatalf("file basename = %q, want %q", filepath.Base(recStart.File), wantBase)
	}

	time.Sleep(300 * time.Millisecond)

	stopResp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s/record", instA.baseURL(), outboundID))
	if stopResp.StatusCode != http.StatusOK {
		t.Fatalf("stop recording: unexpected status %d", stopResp.StatusCode)
	}
	var recStop recordingResponse
	decodeJSON(t, stopResp, &recStop)
	if recStop.File != recStart.File {
		t.Fatalf("file path mismatch: start=%q stop=%q", recStart.File, recStop.File)
	}
	assertWAVAudio(t, recStart.File, 2, 8000, 100)

	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
}

func TestRecording_CustomFilename_RejectsReuse(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	outboundID, _ := establishCall(t, instA, instB)

	name := "reuse-" + outboundID[:8]
	body := map[string]interface{}{"filename": name}
	recURL := fmt.Sprintf("%s/v1/legs/%s/record", instA.baseURL(), outboundID)

	recResp := httpPost(t, recURL, body)
	if recResp.StatusCode != http.StatusOK {
		t.Fatalf("first start: unexpected status %d", recResp.StatusCode)
	}
	var recStart recordingResponse
	decodeJSON(t, recResp, &recStart)

	time.Sleep(200 * time.Millisecond)
	stopResp := httpDelete(t, recURL)
	if stopResp.StatusCode != http.StatusOK {
		t.Fatalf("stop: unexpected status %d", stopResp.StatusCode)
	}
	stopResp.Body.Close()

	first, err := os.ReadFile(recStart.File)
	if err != nil {
		t.Fatalf("ReadFile first recording: %v", err)
	}

	reuseResp := httpPost(t, recURL, body)
	if reuseResp.StatusCode != http.StatusConflict {
		t.Fatalf("reuse start: status %d, want 409", reuseResp.StatusCode)
	}
	reuseResp.Body.Close()

	got, err := os.ReadFile(recStart.File)
	if err != nil {
		t.Fatalf("ReadFile after rejected reuse: %v", err)
	}
	if string(got) != string(first) {
		t.Fatal("first recording was modified after a rejected reuse")
	}

	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
}

func TestRecording_CustomFilename_RejectsInFlightCollision(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	outboundID, _ := establishCall(t, instA, instB)

	roomResp := httpPost(t, instA.baseURL()+"/v1/rooms", map[string]interface{}{})
	if roomResp.StatusCode != http.StatusCreated {
		t.Fatalf("create room: unexpected status %d", roomResp.StatusCode)
	}
	var rm roomView
	decodeJSON(t, roomResp, &rm)

	addResp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/legs", instA.baseURL(), rm.ID), map[string]interface{}{
		"leg_id": outboundID,
	})
	if addResp.StatusCode != http.StatusOK {
		t.Fatalf("add leg to room: unexpected status %d", addResp.StatusCode)
	}
	addResp.Body.Close()
	instA.collector.waitForMatch(t, events.LegJoinedRoom, nil, 3*time.Second)

	name := "shared-" + outboundID[:8]
	body := map[string]interface{}{"filename": name}

	legResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/record", instA.baseURL(), outboundID), body)
	if legResp.StatusCode != http.StatusOK {
		t.Fatalf("leg start: unexpected status %d", legResp.StatusCode)
	}
	var legStart recordingResponse
	decodeJSON(t, legResp, &legStart)

	roomRecResp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/record", instA.baseURL(), rm.ID), body)
	if roomRecResp.StatusCode != http.StatusConflict {
		t.Fatalf("colliding room start: status %d, want 409", roomRecResp.StatusCode)
	}
	roomRecResp.Body.Close()

	if _, err := os.Stat(legStart.File); !os.IsNotExist(err) {
		t.Fatalf("in-flight final path should still be absent, os.Stat err = %v", err)
	}

	stopResp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s/record", instA.baseURL(), outboundID))
	if stopResp.StatusCode != http.StatusOK {
		t.Fatalf("stop: unexpected status %d", stopResp.StatusCode)
	}
	stopResp.Body.Close()

	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
}

func TestRecording_CustomFilename_InvalidRejected(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	outboundID, _ := establishCall(t, instA, instB)

	resp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/record", instA.baseURL(), outboundID), map[string]interface{}{
		"filename": "../secret",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid filename: status %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
}
