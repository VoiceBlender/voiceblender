//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

func assertTurnDetector(t *testing.T, inst *testInstance, legID string, want bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if inst.apiSrv.HasTurnDetector(legID) == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	got := inst.apiSrv.HasTurnDetector(legID)
	t.Fatalf("[%s] leg %s: turn detector attached=%v, want %v", inst.name, legID, got, want)
}

func TestTurnDetection_DisabledByDefault(t *testing.T) {
	instA := newTestInstance(t, "turn-def-a")
	instB := newTestInstance(t, "turn-def-b")

	outboundID, inboundID := establishCall(t, instA, instB)

	assertTurnDetector(t, instA, outboundID, false)
	assertTurnDetector(t, instB, inboundID, false)

	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
}

func TestTurnDetection_EnabledGlobally(t *testing.T) {
	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer mockSrv.Close()

	instA := newTestInstanceWithOpts(t, "turn-glob-a", func(c *config.Config) {
		c.SmartTurnEnabled = true
		c.SmartTurnURL = mockSrv.URL
		c.SmartTurnTransport = "http"
	})
	instB := newTestInstanceWithOpts(t, "turn-glob-b", func(c *config.Config) {
		c.SmartTurnEnabled = true
		c.SmartTurnURL = mockSrv.URL
		c.SmartTurnTransport = "http"
	})

	outboundID, inboundID := establishCall(t, instA, instB)

	assertTurnDetector(t, instA, outboundID, true)
	assertTurnDetector(t, instB, inboundID, true)

	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
}

func TestTurnDetection_PerCallOutboundOverride(t *testing.T) {
	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer mockSrv.Close()

	instA := newTestInstanceWithOpts(t, "turn-out-a", func(c *config.Config) {
		c.SmartTurnURL = mockSrv.URL
		c.SmartTurnTransport = "http"
	})
	instB := newTestInstanceWithOpts(t, "turn-out-b", func(c *config.Config) {
		c.SmartTurnURL = mockSrv.URL
		c.SmartTurnTransport = "http"
	})

	createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type":           "sip",
		"uri":            fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"codecs":         []string{"PCMU"},
		"turn_detection": true,
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: unexpected status %d", createResp.StatusCode)
	}
	var outbound legView
	decodeJSON(t, createResp, &outbound)

	inbound := waitForInboundLeg(t, instB.baseURL(), 5*time.Second)

	answerResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/answer", instB.baseURL(), inbound.ID), nil)
	if answerResp.StatusCode != http.StatusAccepted {
		t.Fatalf("answer: unexpected status %d", answerResp.StatusCode)
	}
	answerResp.Body.Close()

	waitForLegState(t, instA.baseURL(), outbound.ID, "connected", 5*time.Second)
	waitForLegState(t, instB.baseURL(), inbound.ID, "connected", 5*time.Second)

	// Outbound opted in explicitly; inbound used default (disabled).
	assertTurnDetector(t, instA, outbound.ID, true)
	assertTurnDetector(t, instB, inbound.ID, false)

	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outbound.ID))
}

func TestTurnDetection_WSMode_MockConnected(t *testing.T) {
	var connected atomic.Bool
	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/ws/stream" {
			conn, _, _, err := ws.UpgradeHTTP(r, w)
			if err != nil {
				return
			}
			defer conn.Close()
			connected.Store(true)

			// Send one turn decision
			msg, _ := json.Marshal(map[string]interface{}{
				"event":          "turn_decision",
				"action":         "TURN_COMPLETE",
				"probability":    0.88,
				"threshold_used": 0.55,
				"evaluation_ms":  10,
			})
			_ = wsutil.WriteServerText(conn, msg)

			// Read audio until close
			for {
				_, _, err := wsutil.ReadClientData(conn)
				if err != nil {
					return
				}
			}
		}
	}))
	defer mockSrv.Close()

	instA := newTestInstanceWithOpts(t, "turn-ws-a", func(c *config.Config) {
		c.SmartTurnEnabled = true
		c.SmartTurnURL = mockSrv.URL
		c.SmartTurnTransport = "ws"
	})
	instB := newTestInstance(t, "turn-ws-b")

	outboundID, _ := establishCall(t, instA, instB)
	assertTurnDetector(t, instA, outboundID, true)

	// Wait for event collector on instA to pick up TurnComplete
	ev := instA.collector.waitForMatch(t, events.TurnComplete, nil, 3*time.Second)
	if ev.Type != events.TurnComplete {
		t.Fatalf("expected turn.complete event, got %v", ev.Type)
	}
	data, ok := ev.Data.(*events.TurnDetectionData)
	if !ok {
		t.Fatalf("expected *events.TurnDetectionData, got %T", ev.Data)
	}
	if data.Probability != 0.88 || data.Transport != "ws" {
		t.Errorf("unexpected event data: %+v", data)
	}

	if !connected.Load() {
		t.Error("expected mock WebSocket to be connected")
	}

	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
}
