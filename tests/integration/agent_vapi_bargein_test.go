//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/providercfg"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

const (
	vapiMockAPIKey   = "vapi-test-key"
	vapiMockGreeting = "hello from the vapi assistant"
)

// burstVAPIMock stands in for VAPI: POST /call returns a listenUrl on the
// same server, whose WebSocket bursts a long response faster than real time.
type burstVAPIMock struct {
	url string

	mu          sync.Mutex
	conn        net.Conn
	assistantID string
}

func newBurstVAPIMock(t *testing.T) *burstVAPIMock {
	t.Helper()
	m := &burstVAPIMock{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /call", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+vapiMockAPIKey {
			http.Error(w, "bad key", http.StatusUnauthorized)
			return
		}
		var req struct {
			AssistantID string `json:"assistantId"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		m.mu.Lock()
		m.assistantID = req.AssistantID
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"id":         "call-mock",
			"listenUrl":  "ws" + strings.TrimPrefix(m.url, "http") + "/listen",
			"controlUrl": m.url + "/control",
		})
	})
	mux.HandleFunc("GET /listen", func(w http.ResponseWriter, r *http.Request) {
		conn, _, _, err := ws.UpgradeHTTP(r, w)
		if err != nil {
			return
		}
		defer conn.Close()
		m.mu.Lock()
		m.conn = conn
		_ = wsutil.WriteServerText(conn, []byte(fmt.Sprintf(
			`{"type":"transcript","role":"assistant","transcriptType":"final","transcript":%q}`, vapiMockGreeting)))
		const chunk = 100 * time.Millisecond
		for i := 0; i < int(burstAgentSpeech/chunk); i++ {
			_ = wsutil.WriteServerBinary(conn, toneChunk16k(i, chunk))
		}
		m.mu.Unlock()
		for {
			if _, _, err := wsutil.ReadClientData(conn); err != nil {
				return
			}
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	m.url = srv.URL
	return m
}

func (m *burstVAPIMock) interrupt(t *testing.T) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.conn == nil {
		t.Fatal("vapi mock not connected")
	}
	if err := wsutil.WriteServerText(m.conn, []byte(`{"type":"user-interrupted","turnId":"turn-1"}`)); err != nil {
		t.Fatalf("send user-interrupted: %v", err)
	}
}

// TestAgent_VAPIInterruptionDiscardsBufferedSpeech: with VAPI_BASE_URL
// pointed at a mock, the call is created there, an assistant transcript
// reaches agent.agent_response, and user-interrupted cuts the buffered speech
// while the agent stays attached.
func TestAgent_VAPIInterruptionDiscardsBufferedSpeech(t *testing.T) {
	mock := newBurstVAPIMock(t)
	instA := newTestInstanceWithOpts(t, "vapi-barge-a", func(c *config.Config) {
		c.VAPIAPIKey = vapiMockAPIKey
		c.VAPI = providercfg.VAPI{BaseURL: mock.url}
	})
	instB := newTestInstanceWithOpts(t, "vapi-barge-b", func(c *config.Config) { c.SpeechDetectionEnabled = true })
	outboundID, inboundID := establishCall(t, instA, instB)
	legURL := fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID)

	resp := httpPost(t, legURL+"/agent/vapi", map[string]any{"assistant_id": "asst-mock"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("attach agent: status %d", resp.StatusCode)
	}

	instA.collector.waitForMatch(t, events.AgentAgentResponse, func(e events.Event) bool {
		d, ok := e.Data.(*events.AgentResponseData)
		return ok && d.LegID == outboundID && d.Text == vapiMockGreeting
	}, 5*time.Second)
	mock.mu.Lock()
	gotAssistant := mock.assistantID
	mock.mu.Unlock()
	if gotAssistant != "asst-mock" {
		t.Fatalf("mock got assistantId %q, want asst-mock", gotAssistant)
	}

	instB.collector.waitForMatch(t, events.SpeakingStarted, forLeg(inboundID), 5*time.Second)
	time.Sleep(500 * time.Millisecond)

	t0 := time.Now()
	mock.interrupt(t)
	assertAgentSilencedWithin(t, instB, inboundID, t0)

	resp = httpDelete(t, legURL+"/agent")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("agent should still be attached after interruption: status %d", resp.StatusCode)
	}
}
