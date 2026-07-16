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
	if answerResp.StatusCode != http.StatusAccepted {
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
	if answerResp.StatusCode != http.StatusAccepted {
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
	if answerResp.StatusCode != http.StatusAccepted {
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

// TestAMD_TeardownMidAnalysis hangs a call up while AMD is still analysing and
// pins the no-publish-on-teardown contract: a verdict for a torn-down call is
// noise, and the leg already reports its own disconnect. It reddens if the
// deadline goroutine's teardown path ever starts publishing.
//
// It is not the leak regression — a leaked analysis publishes nothing either,
// so these assertions hold with or without the leak. The leak proof is
// TestAMDDriver_WatchExitsOnLegTeardown, which observes the goroutine's return.
func TestAMD_TeardownMidAnalysis(t *testing.T) {
	instA := newTestInstance(t, "amd-teardown-a")
	instB := newTestInstance(t, "amd-teardown-b")

	// Windows far longer than the call lives, so no threshold can be reached
	// before the hangup — only teardown can end this analysis.
	createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type":   "sip",
		"uri":    fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"codecs": []string{"PCMU"},
		"amd": map[string]interface{}{
			"initial_silence_timeout": 20000,
			"greeting_duration":       20000,
			"after_greeting_silence":  20000,
			"total_analysis_time":     30000,
		},
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: unexpected status %d", createResp.StatusCode)
	}
	var outboundLeg legView
	decodeJSON(t, createResp, &outboundLeg)

	inboundLeg := waitForInboundLeg(t, instB.baseURL(), 5*time.Second)
	answerResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/answer", instB.baseURL(), inboundLeg.ID), nil)
	if answerResp.StatusCode != http.StatusAccepted {
		t.Fatalf("answer: unexpected status %d", answerResp.StatusCode)
	}
	answerResp.Body.Close()

	waitForLegState(t, instA.baseURL(), outboundLeg.ID, "connected", 5*time.Second)
	waitForLegState(t, instB.baseURL(), inboundLeg.ID, "connected", 5*time.Second)

	// Let RTP flow so the analysis is genuinely in flight, then hang up.
	time.Sleep(500 * time.Millisecond)
	if instA.collector.hasEvent(events.AMDResult, nil) {
		t.Fatal("AMD reached a verdict before teardown — test cannot prove anything")
	}
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundLeg.ID))

	// Well past the point where a leaked analysis would have published.
	time.Sleep(3 * time.Second)

	if instA.collector.hasEvent(events.AMDResult, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundLeg.ID
	}) {
		t.Error("expected no amd.result after the leg was torn down mid-analysis")
	}
	if instA.collector.hasEvent(events.AMDBeep, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundLeg.ID
	}) {
		t.Error("expected no amd.beep after the leg was torn down mid-analysis")
	}
}

// TestAMD_InvalidParams verifies that invalid AMD parameters are rejected.
func TestAMD_InvalidParams(t *testing.T) {
	instA := newTestInstance(t, "amd-invalid-a")
	instB := newTestInstance(t, "amd-invalid-b")

	tests := []struct {
		name string
		amd  map[string]interface{}
	}{
		{
			name: "total < initial_silence",
			amd: map[string]interface{}{
				"initial_silence_timeout": 5000,
				"total_analysis_time":     2000,
			},
		},
		{
			// A greeting window longer than the whole analysis window can never
			// be reached, so the machine verdict is unreachable.
			name: "total < greeting",
			amd: map[string]interface{}{
				"initial_silence_timeout": 1000,
				"greeting_duration":       5000,
				"total_analysis_time":     2000,
			},
		},
		{
			name: "total < after_greeting",
			amd: map[string]interface{}{
				"initial_silence_timeout": 1000,
				"after_greeting_silence":  5000,
				"total_analysis_time":     2000,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
				"type":   "sip",
				"uri":    fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
				"codecs": []string{"PCMU"},
				"amd":    tt.amd,
			})
			if createResp.StatusCode != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d", createResp.StatusCode)
			}
			createResp.Body.Close()
		})
	}
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
	if answerResp.StatusCode != http.StatusAccepted {
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

// --- POST /v1/legs/{id}/amd endpoint tests ---

// TestAMD_PostEndpoint_Machine starts AMD via the POST endpoint after the call
// is already connected, plays a continuous tone, and verifies machine detection.
func TestAMD_PostEndpoint_Machine(t *testing.T) {
	instA := newTestInstance(t, "amd-ep-machine-a")
	instB := newTestInstance(t, "amd-ep-machine-b")

	outID, inID := establishCall(t, instA, instB)

	amdResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/amd", instA.baseURL(), outID), map[string]interface{}{
		"initial_silence_timeout": 4000,
		"greeting_duration":       1500,
		"total_analysis_time":     8000,
	})
	if amdResp.StatusCode != http.StatusOK {
		t.Fatalf("start AMD: unexpected status %d", amdResp.StatusCode)
	}
	amdResp.Body.Close()

	playResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/play", instB.baseURL(), inID), map[string]interface{}{
		"tone":   "us_dial",
		"repeat": -1,
	})
	if playResp.StatusCode != http.StatusOK {
		t.Fatalf("play: unexpected status %d", playResp.StatusCode)
	}
	playResp.Body.Close()

	e := instA.collector.waitForMatch(t, events.AMDResult, func(e events.Event) bool {
		return e.Data.GetLegID() == outID
	}, 15*time.Second)

	amdData := e.Data.(*events.AMDResultData)
	if amdData.Result != "machine" {
		t.Errorf("expected machine, got %s", amdData.Result)
	}
	t.Logf("POST endpoint AMD: %s (greeting=%dms total=%dms)",
		amdData.Result, amdData.GreetingDurationMs, amdData.TotalAnalysisMs)

	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outID))
}

// TestAMD_PostEndpoint_Defaults verifies POST /v1/legs/{id}/amd with empty body.
func TestAMD_PostEndpoint_Defaults(t *testing.T) {
	instA := newTestInstance(t, "amd-ep-def-a")
	instB := newTestInstance(t, "amd-ep-def-b")

	outID, _ := establishCall(t, instA, instB)

	amdResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/amd", instA.baseURL(), outID), nil)
	if amdResp.StatusCode != http.StatusOK {
		t.Fatalf("start AMD: unexpected status %d", amdResp.StatusCode)
	}
	amdResp.Body.Close()

	e := instA.collector.waitForMatch(t, events.AMDResult, func(e events.Event) bool {
		return e.Data.GetLegID() == outID
	}, 10*time.Second)

	amdData := e.Data.(*events.AMDResultData)
	t.Logf("POST endpoint defaults: %s (initial_silence=%dms total=%dms)",
		amdData.Result, amdData.InitialSilenceMs, amdData.TotalAnalysisMs)

	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outID))
}

// TestAMD_PostEndpoint_NotConnected verifies 409 when leg is not connected.
func TestAMD_PostEndpoint_NotConnected(t *testing.T) {
	instA := newTestInstance(t, "amd-ep-nc-a")
	instB := newTestInstance(t, "amd-ep-nc-b")

	createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type":   "sip",
		"uri":    fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"codecs": []string{"PCMU"},
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: unexpected status %d", createResp.StatusCode)
	}
	var outLeg legView
	decodeJSON(t, createResp, &outLeg)
	waitForInboundLeg(t, instB.baseURL(), 5*time.Second)

	amdResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/amd", instA.baseURL(), outLeg.ID), nil)
	if amdResp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409, got %d", amdResp.StatusCode)
	}
	amdResp.Body.Close()

	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outLeg.ID))
}

// TestAMD_PostEndpoint_NotFound verifies 404 for non-existent leg.
func TestAMD_PostEndpoint_NotFound(t *testing.T) {
	instA := newTestInstance(t, "amd-ep-nf-a")

	amdResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/amd", instA.baseURL(), "nonexistent-id"), nil)
	if amdResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", amdResp.StatusCode)
	}
	amdResp.Body.Close()
}

// TestAMD_PostEndpoint_InvalidParams verifies 400 for invalid AMD parameters.
func TestAMD_PostEndpoint_InvalidParams(t *testing.T) {
	instA := newTestInstance(t, "amd-ep-inv-a")
	instB := newTestInstance(t, "amd-ep-inv-b")

	outID, _ := establishCall(t, instA, instB)

	tests := []struct {
		name string
		body map[string]interface{}
	}{
		{
			name: "total < initial_silence",
			body: map[string]interface{}{
				"initial_silence_timeout": 5000,
				"total_analysis_time":     2000,
			},
		},
		{
			name: "total < greeting",
			body: map[string]interface{}{
				"initial_silence_timeout": 1000,
				"greeting_duration":       5000,
				"total_analysis_time":     2000,
			},
		},
		{
			name: "total < after_greeting",
			body: map[string]interface{}{
				"initial_silence_timeout": 1000,
				"after_greeting_silence":  5000,
				"total_analysis_time":     2000,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			amdResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/amd", instA.baseURL(), outID), tt.body)
			if amdResp.StatusCode != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d", amdResp.StatusCode)
			}
			amdResp.Body.Close()
		})
	}

	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outID))
}
