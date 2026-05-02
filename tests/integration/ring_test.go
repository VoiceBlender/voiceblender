//go:build integration

package integration

import (
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/config"
)

// TestRing_ExplicitRingingThenAnswer verifies that with SIP_AUTO_RINGING off
// (the default), the API caller can drive ringing explicitly via /ring and
// still answer normally.
func TestRing_ExplicitRingingThenAnswer(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b") // SIPAutoRinging defaults to false

	createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type": "sip",
		"uri":  fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: status %d", createResp.StatusCode)
	}
	var outbound legView
	decodeJSON(t, createResp, &outbound)

	inbound := waitForInboundLeg(t, instB.baseURL(), 5*time.Second)

	// Multiple /ring calls are allowed; each emits another 180.
	for i := 0; i < 2; i++ {
		ringResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/ring", instB.baseURL(), inbound.ID), nil)
		if ringResp.StatusCode != http.StatusAccepted {
			body, _ := io.ReadAll(ringResp.Body)
			ringResp.Body.Close()
			t.Fatalf("ring #%d: status %d, body=%s", i+1, ringResp.StatusCode, body)
		}
		ringResp.Body.Close()
	}

	answerResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/answer", instB.baseURL(), inbound.ID), nil)
	if answerResp.StatusCode != http.StatusAccepted {
		t.Fatalf("answer: status %d", answerResp.StatusCode)
	}
	answerResp.Body.Close()

	waitForLegState(t, instA.baseURL(), outbound.ID, "connected", 5*time.Second)
	waitForLegState(t, instB.baseURL(), inbound.ID, "connected", 5*time.Second)
}

// TestRing_AutoRingingPreservesLegacyFlow verifies that the original
// "ring immediately on INVITE" behavior is still reachable via opt-in.
func TestRing_AutoRingingPreservesLegacyFlow(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstanceWithOpts(t, "instance-b", func(c *config.Config) {
		c.SIPAutoRinging = true
	})

	createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type": "sip",
		"uri":  fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: status %d", createResp.StatusCode)
	}
	var outbound legView
	decodeJSON(t, createResp, &outbound)

	inbound := waitForInboundLeg(t, instB.baseURL(), 5*time.Second)

	// No /ring call needed — the engine should have sent 180 automatically.
	answerResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/answer", instB.baseURL(), inbound.ID), nil)
	if answerResp.StatusCode != http.StatusAccepted {
		t.Fatalf("answer: status %d", answerResp.StatusCode)
	}
	answerResp.Body.Close()

	waitForLegState(t, instA.baseURL(), outbound.ID, "connected", 5*time.Second)
}

// TestRing_RejectsAfterAnswer verifies /ring returns 409 once the leg has
// transitioned out of ringing state.
func TestRing_RejectsAfterAnswer(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")

	createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type":   "sip",
		"uri":    fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"codecs": []string{"PCMU"},
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: status %d", createResp.StatusCode)
	}
	createResp.Body.Close()

	inbound := waitForInboundLeg(t, instB.baseURL(), 5*time.Second)

	answerResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/answer", instB.baseURL(), inbound.ID), nil)
	if answerResp.StatusCode != http.StatusAccepted {
		t.Fatalf("answer: status %d", answerResp.StatusCode)
	}
	answerResp.Body.Close()

	waitForLegState(t, instB.baseURL(), inbound.ID, "connected", 5*time.Second)

	resp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/ring", instB.baseURL(), inbound.ID), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("ring on connected leg: status %d, body=%s, want 409", resp.StatusCode, body)
	}
}
