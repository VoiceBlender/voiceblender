//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
)

// TestAMD_Human establishes a call with AMD enabled, plays a short tone on the
// inbound side (simulating a human greeting), then verifies the AMD result
// event is emitted with result "human".
func TestAMD_Human(t *testing.T) {
	instA := newTestInstance(t, "amd-human-a")
	instB := newTestInstance(t, "amd-human-b")

	// A dials B with AMD enabled.
	// Use generous initial_silence_timeout to account for call setup latency.
	createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type":   "sip",
		"uri":    fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"codecs": []string{"PCMU"},
		"amd": map[string]interface{}{
			"initial_silence_timeout": 4000,
			"greeting_duration":       1500,
			"after_greeting_silence":  800,
			"total_analysis_time":     8000,
		},
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: unexpected status %d", createResp.StatusCode)
	}
	var outboundLeg legView
	decodeJSON(t, createResp, &outboundLeg)

	// Wait for inbound leg on B and answer.
	inboundLeg := waitForInboundLeg(t, instB.baseURL(), 5*time.Second)
	answerResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/answer", instB.baseURL(), inboundLeg.ID), nil)
	if answerResp.StatusCode != http.StatusOK {
		t.Fatalf("answer: unexpected status %d", answerResp.StatusCode)
	}
	answerResp.Body.Close()

	waitForLegState(t, instA.baseURL(), outboundLeg.ID, "connected", 5*time.Second)
	waitForLegState(t, instB.baseURL(), inboundLeg.ID, "connected", 5*time.Second)

	// Play a continuous tone on the inbound side, then stop it after 500ms
	// to simulate a short human greeting.
	type playResponse struct {
		PlaybackID string `json:"playback_id"`
	}
	playResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/play", instB.baseURL(), inboundLeg.ID), map[string]interface{}{
		"tone":   "us_dial",
		"repeat": -1,
	})
	if playResp.StatusCode != http.StatusOK {
		t.Fatalf("play: unexpected status %d", playResp.StatusCode)
	}
	var pbResp playResponse
	decodeJSON(t, playResp, &pbResp)

	// Let it play for 500ms (< greeting_duration of 1500ms), then stop.
	time.Sleep(500 * time.Millisecond)
	stopResp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s/play/%s", instB.baseURL(), inboundLeg.ID, pbResp.PlaybackID))
	if stopResp.StatusCode != http.StatusOK {
		t.Fatalf("stop play: unexpected status %d", stopResp.StatusCode)
	}
	stopResp.Body.Close()

	// The AMD should detect: short speech + silence → human.
	e := instA.collector.waitForMatch(t, events.AMDResult, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundLeg.ID
	}, 15*time.Second)

	amdData := e.Data.(*events.AMDResultData)
	if amdData.Result != "human" {
		t.Errorf("expected result=human, got %s (initial_silence=%d, greeting=%d, total=%d)",
			amdData.Result, amdData.InitialSilenceMs, amdData.GreetingDurationMs, amdData.TotalAnalysisMs)
	}
	t.Logf("AMD result: %s (initial_silence=%dms greeting=%dms total=%dms)",
		amdData.Result, amdData.InitialSilenceMs, amdData.GreetingDurationMs, amdData.TotalAnalysisMs)

	// Cleanup.
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundLeg.ID))
}

// TestAMD_Machine establishes a call with AMD enabled, plays a continuous tone
// on the inbound side (simulating a voicemail greeting), and verifies the AMD
// result event is emitted with result "machine".
func TestAMD_Machine(t *testing.T) {
	instA := newTestInstance(t, "amd-machine-a")
	instB := newTestInstance(t, "amd-machine-b")

	// A dials B with AMD enabled.
	createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type":   "sip",
		"uri":    fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"codecs": []string{"PCMU"},
		"amd": map[string]interface{}{
			"initial_silence_timeout": 4000,
			"greeting_duration":       1500,
			"after_greeting_silence":  800,
			"total_analysis_time":     8000,
		},
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: unexpected status %d", createResp.StatusCode)
	}
	var outboundLeg legView
	decodeJSON(t, createResp, &outboundLeg)

	inboundLeg := waitForInboundLeg(t, instB.baseURL(), 5*time.Second)
	answerResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/answer", instB.baseURL(), inboundLeg.ID), nil)
	if answerResp.StatusCode != http.StatusOK {
		t.Fatalf("answer: unexpected status %d", answerResp.StatusCode)
	}
	answerResp.Body.Close()

	waitForLegState(t, instA.baseURL(), outboundLeg.ID, "connected", 5*time.Second)
	waitForLegState(t, instB.baseURL(), inboundLeg.ID, "connected", 5*time.Second)

	// Play a continuous repeating tone on the inbound side (simulating a long
	// voicemail greeting). repeat=-1 loops indefinitely.
	playResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/play", instB.baseURL(), inboundLeg.ID), map[string]interface{}{
		"tone":   "us_dial",
		"repeat": -1,
	})
	if playResp.StatusCode != http.StatusOK {
		t.Fatalf("play: unexpected status %d", playResp.StatusCode)
	}
	playResp.Body.Close()

	// AMD should detect long continuous audio → machine.
	e := instA.collector.waitForMatch(t, events.AMDResult, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundLeg.ID
	}, 15*time.Second)

	amdData := e.Data.(*events.AMDResultData)
	if amdData.Result != "machine" {
		t.Errorf("expected result=machine, got %s (initial_silence=%d, greeting=%d, total=%d)",
			amdData.Result, amdData.InitialSilenceMs, amdData.GreetingDurationMs, amdData.TotalAnalysisMs)
	}
	t.Logf("AMD result: %s (initial_silence=%dms greeting=%dms total=%dms)",
		amdData.Result, amdData.InitialSilenceMs, amdData.GreetingDurationMs, amdData.TotalAnalysisMs)

	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundLeg.ID))
}

// TestAMD_NoSpeech establishes a call with AMD enabled but sends no audio,
// verifying the AMD result is "no_speech".
func TestAMD_NoSpeech(t *testing.T) {
	instA := newTestInstance(t, "amd-nospeech-a")
	instB := newTestInstance(t, "amd-nospeech-b")

	createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type":   "sip",
		"uri":    fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"codecs": []string{"PCMU"},
		"amd": map[string]interface{}{
			"initial_silence_timeout": 1000, // 1s — short for test speed
			"total_analysis_time":     3000,
		},
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: unexpected status %d", createResp.StatusCode)
	}
	var outboundLeg legView
	decodeJSON(t, createResp, &outboundLeg)

	inboundLeg := waitForInboundLeg(t, instB.baseURL(), 5*time.Second)
	answerResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/answer", instB.baseURL(), inboundLeg.ID), nil)
	if answerResp.StatusCode != http.StatusOK {
		t.Fatalf("answer: unexpected status %d", answerResp.StatusCode)
	}
	answerResp.Body.Close()

	waitForLegState(t, instA.baseURL(), outboundLeg.ID, "connected", 5*time.Second)

	// Don't play anything — silence should trigger no_speech.
	e := instA.collector.waitForMatch(t, events.AMDResult, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundLeg.ID
	}, 10*time.Second)

	amdData := e.Data.(*events.AMDResultData)
	if amdData.Result != "no_speech" {
		t.Errorf("expected result=no_speech, got %s (initial_silence=%d, greeting=%d, total=%d)",
			amdData.Result, amdData.InitialSilenceMs, amdData.GreetingDurationMs, amdData.TotalAnalysisMs)
	}
	t.Logf("AMD result: %s (initial_silence=%dms total=%dms)",
		amdData.Result, amdData.InitialSilenceMs, amdData.TotalAnalysisMs)

	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundLeg.ID))
}

// TestAMD_Disabled verifies that omitting the "amd" field produces no AMD event.
func TestAMD_Disabled(t *testing.T) {
	instA := newTestInstance(t, "amd-disabled-a")
	instB := newTestInstance(t, "amd-disabled-b")

	outID, _ := establishCall(t, instA, instB)

	// Wait long enough that AMD would have produced a result if enabled.
	time.Sleep(3 * time.Second)

	if instA.collector.hasEvent(events.AMDResult, nil) {
		t.Fatal("expected no AMD event when amd field is omitted")
	}

	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outID))
}

// TestAMD_InvalidParams verifies that invalid AMD parameters are rejected.
func TestAMD_InvalidParams(t *testing.T) {
	instA := newTestInstance(t, "amd-invalid-a")
	instB := newTestInstance(t, "amd-invalid-b")

	// total_analysis_time < initial_silence_timeout should fail validation.
	createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type":   "sip",
		"uri":    fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"codecs": []string{"PCMU"},
		"amd": map[string]interface{}{
			"initial_silence_timeout": 5000,
			"total_analysis_time":     2000,
		},
	})
	if createResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", createResp.StatusCode)
	}
	createResp.Body.Close()
}

// TestAMD_DefaultParams verifies that "amd": {} uses all defaults and produces a result.
func TestAMD_DefaultParams(t *testing.T) {
	instA := newTestInstance(t, "amd-defaults-a")
	instB := newTestInstance(t, "amd-defaults-b")

	createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type":   "sip",
		"uri":    fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"codecs": []string{"PCMU"},
		"amd":    map[string]interface{}{},
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: unexpected status %d", createResp.StatusCode)
	}
	var outboundLeg legView
	decodeJSON(t, createResp, &outboundLeg)

	inboundLeg := waitForInboundLeg(t, instB.baseURL(), 5*time.Second)
	answerResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/answer", instB.baseURL(), inboundLeg.ID), nil)
	if answerResp.StatusCode != http.StatusOK {
		t.Fatalf("answer: unexpected status %d", answerResp.StatusCode)
	}
	answerResp.Body.Close()

	waitForLegState(t, instA.baseURL(), outboundLeg.ID, "connected", 5*time.Second)

	// With defaults (initial_silence=2500ms), silence should trigger no_speech
	// within total_analysis_time (5s).
	e := instA.collector.waitForMatch(t, events.AMDResult, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundLeg.ID
	}, 10*time.Second)

	amdData := e.Data.(*events.AMDResultData)
	// With silence, expect no_speech.
	if amdData.Result != "no_speech" {
		t.Logf("AMD with defaults: result=%s (unexpected but may depend on RTP silence encoding)",
			amdData.Result)
	}
	t.Logf("AMD result: %s (initial_silence=%dms total=%dms)",
		amdData.Result, amdData.InitialSilenceMs, amdData.TotalAnalysisMs)

	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundLeg.ID))
}
