//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/config"
)

func assertDetector(t *testing.T, inst *testInstance, legID string, want bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if inst.apiSrv.HasSpeakingDetector(legID) == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	got := inst.apiSrv.HasSpeakingDetector(legID)
	t.Fatalf("[%s] leg %s: detector attached=%v, want %v", inst.name, legID, got, want)
}

func TestSpeechDetection_DisabledByDefault(t *testing.T) {
	instA := newTestInstance(t, "speech-def-a")
	instB := newTestInstance(t, "speech-def-b")

	outboundID, inboundID := establishCall(t, instA, instB)

	assertDetector(t, instA, outboundID, false)
	assertDetector(t, instB, inboundID, false)

	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
}

func TestSpeechDetection_EnabledGlobally(t *testing.T) {
	instA := newTestInstanceWithOpts(t, "speech-glob-a", func(c *config.Config) { c.SpeechDetectionEnabled = true })
	instB := newTestInstanceWithOpts(t, "speech-glob-b", func(c *config.Config) { c.SpeechDetectionEnabled = true })

	outboundID, inboundID := establishCall(t, instA, instB)

	assertDetector(t, instA, outboundID, true)
	assertDetector(t, instB, inboundID, true)

	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
}

func TestSpeechDetection_PerCallOutboundOverride(t *testing.T) {
	instA := newTestInstance(t, "speech-out-a")
	instB := newTestInstance(t, "speech-out-b")

	createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type":             "sip",
		"uri":              fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"codecs":           []string{"PCMU"},
		"speech_detection": true,
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: unexpected status %d", createResp.StatusCode)
	}
	var outbound legView
	decodeJSON(t, createResp, &outbound)

	inbound := waitForInboundLeg(t, instB.baseURL(), 5*time.Second)

	answerResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/answer", instB.baseURL(), inbound.ID), nil)
	if answerResp.StatusCode != http.StatusOK {
		t.Fatalf("answer: unexpected status %d", answerResp.StatusCode)
	}
	answerResp.Body.Close()

	waitForLegState(t, instA.baseURL(), outbound.ID, "connected", 5*time.Second)
	waitForLegState(t, instB.baseURL(), inbound.ID, "connected", 5*time.Second)

	// Outbound opted in explicitly; inbound used default (disabled).
	assertDetector(t, instA, outbound.ID, true)
	assertDetector(t, instB, inbound.ID, false)

	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outbound.ID))
}

func TestSpeechDetection_PerCallAnswerOverride(t *testing.T) {
	instA := newTestInstanceWithOpts(t, "speech-ans-a", func(c *config.Config) { c.SpeechDetectionEnabled = true })
	instB := newTestInstanceWithOpts(t, "speech-ans-b", func(c *config.Config) { c.SpeechDetectionEnabled = true })

	createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type":             "sip",
		"uri":              fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"codecs":           []string{"PCMU"},
		"speech_detection": false,
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: unexpected status %d", createResp.StatusCode)
	}
	var outbound legView
	decodeJSON(t, createResp, &outbound)

	inbound := waitForInboundLeg(t, instB.baseURL(), 5*time.Second)

	answerResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/answer", instB.baseURL(), inbound.ID), map[string]interface{}{
		"speech_detection": false,
	})
	if answerResp.StatusCode != http.StatusOK {
		t.Fatalf("answer: unexpected status %d", answerResp.StatusCode)
	}
	answerResp.Body.Close()

	waitForLegState(t, instA.baseURL(), outbound.ID, "connected", 5*time.Second)
	waitForLegState(t, instB.baseURL(), inbound.ID, "connected", 5*time.Second)

	// Both legs explicitly opted out even though the global default is on.
	assertDetector(t, instA, outbound.ID, false)
	assertDetector(t, instB, inbound.ID, false)

	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outbound.ID))
}
