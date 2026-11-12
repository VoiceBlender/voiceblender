//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/providercfg"
)

// A stopped session returns on the leg's next audio frame, 20 ms at most on
// a live call. This leaves it ample time to return and clean up.
const swapSettle = 500 * time.Millisecond

func newSwapCall(t *testing.T, name string) (mock *vendorOriginMock, instA *testInstance, legID string) {
	t.Helper()
	mock = newVendorOriginMock(t)
	instA = newTestInstanceWithOpts(t, name+"-a", func(c *config.Config) {
		c.DeepgramAPIKey = "dg-test-key"
		c.ElevenLabsAPIKey = "el-test-key"
		c.Deepgram = providercfg.Deepgram{BaseURL: mock.url}
		c.ElevenLabs = providercfg.ElevenLabs{BaseURL: mock.url}
	})
	instB := newTestInstance(t, name+"-b")
	legID, _ = establishCall(t, instA, instB)
	return mock, instA, legID
}

func swapPost(t *testing.T, url string, body map[string]any) {
	t.Helper()
	resp := httpPost(t, url, body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s: status %d", url, resp.StatusCode)
	}
}

func swapDelete(t *testing.T, url, what string) {
	t.Helper()
	resp := httpDelete(t, url)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: DELETE %s: status %d", what, url, resp.StatusCode)
	}
}

// TestAgent_LegSwapKeepsNewAgent detaches a leg's agent and at once attaches
// another. The first session's exit must not detach the second agent.
func TestAgent_LegSwapKeepsNewAgent(t *testing.T) {
	mock, instA, legID := newSwapCall(t, "agent-swap-leg")
	legURL := fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), legID)

	swapPost(t, legURL+"/agent/deepgram", map[string]any{})
	waitForCond(t, 10*time.Second, "first agent's Settings frame", func() bool {
		return mock.frameFor("/v1/agent/converse") != ""
	})
	swapDelete(t, legURL+"/agent", "detach first agent")

	swapPost(t, legURL+"/agent/elevenlabs", map[string]any{"agent_id": "agent-123"})
	waitForCond(t, 10*time.Second, "second agent to dial", func() bool {
		return mock.uriFor("/v1/convai/conversation") != ""
	})
	time.Sleep(swapSettle)
	swapDelete(t, legURL+"/agent", "second agent should still be attached")
}

// TestAgent_RoomSwapKeepsNewAgent is the room form of the leg swap test.
func TestAgent_RoomSwapKeepsNewAgent(t *testing.T) {
	mock, instA, legID := newSwapCall(t, "agent-swap-room")

	roomResp := httpPost(t, instA.baseURL()+"/v1/rooms", map[string]any{})
	if roomResp.StatusCode != http.StatusCreated {
		t.Fatalf("create room: status %d", roomResp.StatusCode)
	}
	var rm roomView
	decodeJSON(t, roomResp, &rm)
	roomURL := fmt.Sprintf("%s/v1/rooms/%s", instA.baseURL(), rm.ID)
	swapPost(t, roomURL+"/legs", map[string]any{"leg_id": legID})

	swapPost(t, roomURL+"/agent/deepgram", map[string]any{})
	waitForCond(t, 10*time.Second, "first agent's Settings frame", func() bool {
		return mock.frameFor("/v1/agent/converse") != ""
	})
	swapDelete(t, roomURL+"/agent", "detach first agent")

	swapPost(t, roomURL+"/agent/elevenlabs", map[string]any{"agent_id": "agent-123"})
	waitForCond(t, 10*time.Second, "second agent to dial", func() bool {
		return mock.uriFor("/v1/convai/conversation") != ""
	})
	time.Sleep(swapSettle)
	swapDelete(t, roomURL+"/agent", "second agent should still be attached")
}
