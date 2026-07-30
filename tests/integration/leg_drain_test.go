//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// servePrompt serves a 1-second 8kHz WAV over HTTP for the duration of the test.
func servePrompt(t *testing.T) string {
	t.Helper()
	wav := wavOneSecond8k()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/wav")
		w.Write(wav)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/prompt.wav"
}

// startPrompt starts the 1s prompt on legID and returns its playback id.
func startPrompt(t *testing.T, baseURL, legID, promptURL string) string {
	t.Helper()
	resp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/play", baseURL, legID), map[string]interface{}{
		"url":       promptURL,
		"mime_type": "audio/wav",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("play: unexpected status %d", resp.StatusCode)
	}
	var pb struct {
		PlaybackID string `json:"playback_id"`
	}
	decodeJSON(t, resp, &pb)
	return pb.PlaybackID
}

// TestDeleteLegDrain_PromptFinishes is the end-to-end case on a real SIP call:
// a farewell prompt survives a DELETE issued while it is still playing, because
// the request asked for a bounded drain.
func TestDeleteLegDrain_PromptFinishes(t *testing.T) {
	instA := newTestInstance(t, "drain-on-a")
	instB := newTestInstance(t, "drain-on-b")
	outID, _ := establishCall(t, instA, instB)

	promptURL := servePrompt(t)
	conn := dialVSI(t, instA)
	defer conn.Close()

	playbackID := startPrompt(t, instA.baseURL(), outID, promptURL)

	// Well inside the 1s prompt, so without the drain the cut is unmistakable.
	time.Sleep(100 * time.Millisecond)

	delResp := httpDeleteWithBody(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outID), map[string]interface{}{
		"drain_playback":   true,
		"drain_timeout_ms": 5000,
	})
	if delResp.StatusCode != http.StatusAccepted {
		t.Fatalf("delete: unexpected status %d", delResp.StatusCode)
	}
	delResp.Body.Close()

	fin := awaitPlaybackFinished(t, conn, playbackID, 15*time.Second)
	if fin.Reason != "completed" {
		t.Errorf("reason = %q, want %q — the drain did not hold off the hangup", fin.Reason, "completed")
	}
	if fin.PlayedMs < 950 {
		t.Errorf("played_ms = %d, want >= 950 for a 1s prompt allowed to finish", fin.PlayedMs)
	}
	t.Logf("drained delete: reason=%q played_ms=%d", fin.Reason, fin.PlayedMs)
}

// TestDeleteLegNoDrain_PromptTruncated is the paired control: the identical
// flow with the drain fields omitted must still cut the prompt. Without it, a
// passing drained case could just be a harness that never truncates anything.
func TestDeleteLegNoDrain_PromptTruncated(t *testing.T) {
	instA := newTestInstance(t, "drain-off-a")
	instB := newTestInstance(t, "drain-off-b")
	outID, _ := establishCall(t, instA, instB)

	promptURL := servePrompt(t)
	conn := dialVSI(t, instA)
	defer conn.Close()

	playbackID := startPrompt(t, instA.baseURL(), outID, promptURL)
	time.Sleep(100 * time.Millisecond)

	delResp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outID))
	if delResp.StatusCode != http.StatusAccepted {
		t.Fatalf("delete: unexpected status %d", delResp.StatusCode)
	}
	delResp.Body.Close()

	fin := awaitPlaybackFinished(t, conn, playbackID, 15*time.Second)
	if fin.Reason != "stopped" {
		t.Errorf("reason = %q, want %q for a DELETE with no drain opt-in", fin.Reason, "stopped")
	}
	if fin.PlayedMs >= 500 {
		t.Errorf("played_ms = %d, want < 500 for a prompt cut ~100ms in", fin.PlayedMs)
	}
	t.Logf("undrained delete: reason=%q played_ms=%d", fin.Reason, fin.PlayedMs)
}
