//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// TestAgent_LegDefaultProvider verifies that attaching an agent without a
// provider field defaults to "elevenlabs" and returns 503 when no API key
// is configured.
func TestAgent_LegDefaultProvider(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	outboundID, _ := establishCall(t, instA, instB)

	resp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/agent/elevenlabs", instA.baseURL(), outboundID), map[string]interface{}{
		"agent_id": "test-agent",
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", resp.StatusCode)
	}

	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body["error"] != "no elevenlabs API key provided" {
		t.Fatalf("unexpected error: %s", body["error"])
	}
}

// TestAgent_LegVAPIProvider verifies that provider=vapi uses the VAPI API
// key path and returns 503 when no VAPI key is configured.
func TestAgent_LegVAPIProvider(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	outboundID, _ := establishCall(t, instA, instB)

	resp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/agent/vapi", instA.baseURL(), outboundID), map[string]interface{}{
		"assistant_id": "test-agent",
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", resp.StatusCode)
	}

	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body["error"] != "no vapi API key provided" {
		t.Fatalf("unexpected error: %s", body["error"])
	}
}

// TestAgent_RoomDefaultProvider verifies that attaching an agent to a room
// without a provider field defaults to "elevenlabs" and returns 503 when
// no API key is configured.
func TestAgent_RoomDefaultProvider(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	outboundID, _ := establishCall(t, instA, instB)

	// Create a room and add the leg.
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

	resp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/agent/elevenlabs", instA.baseURL(), rm.ID), map[string]interface{}{
		"agent_id": "test-agent",
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", resp.StatusCode)
	}

	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body["error"] != "no elevenlabs API key provided" {
		t.Fatalf("unexpected error: %s", body["error"])
	}
}

// TestAgent_RoomVAPIProvider verifies that provider=vapi on a room agent
// uses the VAPI API key path and returns 503 when no key is configured.
func TestAgent_RoomVAPIProvider(t *testing.T) {
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

	resp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/agent/vapi", instA.baseURL(), rm.ID), map[string]interface{}{
		"assistant_id": "test-agent",
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", resp.StatusCode)
	}

	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body["error"] != "no vapi API key provided" {
		t.Fatalf("unexpected error: %s", body["error"])
	}
}

// TestAgent_LegNotFound verifies 404 when agent is attached to a non-existent leg.
func TestAgent_LegNotFound(t *testing.T) {
	instA := newTestInstance(t, "instance-a")

	resp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/agent/elevenlabs", instA.baseURL(), "nonexistent"), map[string]interface{}{
		"agent_id": "test-agent",
		"api_key":  "fake-key",
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

// TestAgent_LegMissingAgentID verifies 400 when agent_id is not provided.
func TestAgent_LegMissingAgentID(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	outboundID, _ := establishCall(t, instA, instB)

	resp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/agent/elevenlabs", instA.baseURL(), outboundID), map[string]interface{}{
		"api_key": "fake-key",
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

// TestAgent_StopNoAgent verifies 404 when stopping an agent on a leg with no agent.
func TestAgent_StopNoAgent(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	outboundID, _ := establishCall(t, instA, instB)

	resp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s/agent", instA.baseURL(), outboundID))
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

// TestSTT_LegDefaultProvider verifies that STT on a leg defaults to ElevenLabs
// and returns 503 when no API key is configured.
func TestSTT_LegDefaultProvider(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	outboundID, _ := establishCall(t, instA, instB)

	resp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/stt", instA.baseURL(), outboundID), map[string]interface{}{})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", resp.StatusCode)
	}
}

// TestAgent_LegPipecatProvider verifies that provider=pipecat skips API key
// validation and attempts to connect (fails since there's no bot running, but
// the agent should start without a 503 error).
func TestAgent_LegPipecatProvider(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	outboundID, _ := establishCall(t, instA, instB)

	resp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/agent/pipecat", instA.baseURL(), outboundID), map[string]interface{}{
		"websocket_url": "ws://127.0.0.1:19999", // no bot running here
	})
	defer resp.Body.Close()

	// Pipecat doesn't require an API key, so it should return 200 (agent_started)
	// even though the connection will fail asynchronously.
	if resp.StatusCode != http.StatusOK {
		var body map[string]string
		json.NewDecoder(resp.Body).Decode(&body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body["error"])
	}

	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["status"] != "agent_started" {
		t.Fatalf("expected status=agent_started, got %v", body["status"])
	}

	// Wait briefly for the session to fail (no bot at that URL).
	time.Sleep(500 * time.Millisecond)

	// Stop should be safe even after async failure.
	stopResp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s/agent", instA.baseURL(), outboundID))
	stopResp.Body.Close()
	// May be 404 (already cleaned up) or 200 — both are acceptable.
}

// TestAgent_RoomPipecatProvider verifies that provider=pipecat on a room agent
// skips API key validation.
func TestAgent_RoomPipecatProvider(t *testing.T) {
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

	resp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/agent/pipecat", instA.baseURL(), rm.ID), map[string]interface{}{
		"websocket_url": "ws://127.0.0.1:19999",
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var body map[string]string
		json.NewDecoder(resp.Body).Decode(&body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body["error"])
	}

	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["status"] != "agent_started" {
		t.Fatalf("expected status=agent_started, got %v", body["status"])
	}

	// Cleanup.
	time.Sleep(500 * time.Millisecond)
	stopResp := httpDelete(t, fmt.Sprintf("%s/v1/rooms/%s/agent", instA.baseURL(), rm.ID))
	stopResp.Body.Close()
}

// TestSTT_RoomDefaultProvider verifies that STT on a room defaults to ElevenLabs
// and returns 503 when no API key is configured.
func TestSTT_RoomDefaultProvider(t *testing.T) {
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

	// Wait a moment for room participant setup.
	time.Sleep(200 * time.Millisecond)

	resp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/stt", instA.baseURL(), rm.ID), map[string]interface{}{})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", resp.StatusCode)
	}
}

// TestTTS_LegDefaultProvider verifies that TTS on a leg defaults to ElevenLabs
// and returns 503 when no API key is configured.
func TestTTS_LegDefaultProvider(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	outboundID, _ := establishCall(t, instA, instB)

	resp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/tts", instA.baseURL(), outboundID), map[string]interface{}{
		"text":  "Hello",
		"voice": "Rachel",
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", resp.StatusCode)
	}

	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body["error"] != "no elevenlabs API key provided" {
		t.Fatalf("unexpected error: %s", body["error"])
	}
}

// TestTTS_LegAWSProvider verifies that provider=aws does not require an API key
// (uses default AWS credential chain). The synthesis will fail at the AWS call
// level, but the provider selection and API key bypass should succeed, returning
// 200 (playback starts asynchronously).
func TestTTS_LegAWSProvider(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	outboundID, _ := establishCall(t, instA, instB)

	resp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/tts", instA.baseURL(), outboundID), map[string]interface{}{
		"text":     "Hello from Polly",
		"voice":    "Joanna",
		"provider": "aws",
	})
	defer resp.Body.Close()

	// AWS provider doesn't require an API key, so should return 200.
	if resp.StatusCode != http.StatusOK {
		var body map[string]string
		json.NewDecoder(resp.Body).Decode(&body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body["error"])
	}

	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["status"] != "playing" {
		t.Fatalf("expected status=playing, got %v", body["status"])
	}
}

// TestTTS_LegGoogleProvider verifies that provider=google does not require an
// API key (uses Application Default Credentials). The synthesis will fail at
// the Google API level, but the provider selection should succeed, returning
// 200 (playback starts asynchronously).
func TestTTS_LegGoogleProvider(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	outboundID, _ := establishCall(t, instA, instB)

	resp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/tts", instA.baseURL(), outboundID), map[string]interface{}{
		"text":     "Hello from Google",
		"voice":    "en-US-Neural2-F",
		"provider": "google",
	})
	defer resp.Body.Close()

	// Google provider doesn't require an API key, so should return 200.
	if resp.StatusCode != http.StatusOK {
		var body map[string]string
		json.NewDecoder(resp.Body).Decode(&body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body["error"])
	}

	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["status"] != "playing" {
		t.Fatalf("expected status=playing, got %v", body["status"])
	}
}

// TestTTS_RoomGoogleProvider verifies provider=google works for room TTS.
func TestTTS_RoomGoogleProvider(t *testing.T) {
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

	resp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/tts", instA.baseURL(), rm.ID), map[string]interface{}{
		"text":     "Room announcement from Google",
		"voice":    "en-US-Neural2-F",
		"provider": "google",
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var body map[string]string
		json.NewDecoder(resp.Body).Decode(&body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body["error"])
	}

	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["status"] != "playing" {
		t.Fatalf("expected status=playing, got %v", body["status"])
	}
}

// TestTTS_RoomAWSProvider verifies provider=aws works for room TTS.
func TestTTS_RoomAWSProvider(t *testing.T) {
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

	resp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/tts", instA.baseURL(), rm.ID), map[string]interface{}{
		"text":     "Room announcement from Polly",
		"voice":    "Matthew",
		"provider": "aws",
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var body map[string]string
		json.NewDecoder(resp.Body).Decode(&body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body["error"])
	}

	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["status"] != "playing" {
		t.Fatalf("expected status=playing, got %v", body["status"])
	}
}
