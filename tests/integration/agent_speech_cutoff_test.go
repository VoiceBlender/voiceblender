//go:build integration

package integration

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/providercfg"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

// burstAgentMock is an ElevenLabs ConvAI stand-in that, like the real service,
// delivers a long response far faster than real time on connect.
type burstAgentMock struct {
	url string

	mu   sync.Mutex
	conn net.Conn
}

const burstAgentSpeech = 6 * time.Second

func newBurstAgentMock(t *testing.T) *burstAgentMock {
	t.Helper()
	m := &burstAgentMock{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, _, err := ws.UpgradeHTTP(r, w)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := wsutil.ReadClientData(conn); err != nil {
			return
		}
		m.mu.Lock()
		m.conn = conn
		_ = wsutil.WriteServerText(conn, []byte(`{"type":"conversation_initiation_metadata","conversation_initiation_metadata_event":{"conversation_id":"conv-burst"}}`))
		const chunk = 100 * time.Millisecond
		for i := 0; i < int(burstAgentSpeech/chunk); i++ {
			pcm := toneChunk16k(i, chunk)
			msg := fmt.Sprintf(`{"type":"audio","audio_event":{"audio_base_64":%q,"event_id":1}}`, base64.StdEncoding.EncodeToString(pcm))
			_ = wsutil.WriteServerText(conn, []byte(msg))
		}
		m.mu.Unlock()
		for {
			if _, _, err := wsutil.ReadClientData(conn); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	m.url = srv.URL
	return m
}

func (m *burstAgentMock) interrupt(t *testing.T) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.conn == nil {
		t.Fatal("agent mock not connected")
	}
	if err := wsutil.WriteServerText(m.conn, []byte(`{"type":"interruption","interruption_event":{"event_id":1}}`)); err != nil {
		t.Fatalf("send interruption: %v", err)
	}
}

func toneChunk16k(index int, d time.Duration) []byte {
	n := int(16000 * d / time.Second)
	out := make([]byte, n*2)
	for i := 0; i < n; i++ {
		v := 0.3 * math.Sin(2*math.Pi*440*float64(index*n+i)/16000)
		binary.LittleEndian.PutUint16(out[i*2:], uint16(int16(v*32767)))
	}
	return out
}

// startBurstAgentCall attaches the burst agent to A's leg and waits until B
// hears it.
func startBurstAgentCall(t *testing.T, name string) (instA, instB *testInstance, legURL, inboundID string, mock *burstAgentMock) {
	t.Helper()
	mock = newBurstAgentMock(t)
	instA = newTestInstanceWithOpts(t, name+"-a", func(c *config.Config) {
		c.ElevenLabsAPIKey = "el-test-key"
		c.ElevenLabs = providercfg.ElevenLabs{BaseURL: mock.url}
	})
	instB = newTestInstanceWithOpts(t, name+"-b", func(c *config.Config) { c.SpeechDetectionEnabled = true })
	outboundID, inboundID := establishCall(t, instA, instB)
	legURL = fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID)

	resp := httpPost(t, legURL+"/agent/elevenlabs", map[string]any{"agent_id": "burst"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("attach agent: status %d", resp.StatusCode)
	}
	instB.collector.waitForMatch(t, events.SpeakingStarted, forLeg(inboundID), 5*time.Second)
	time.Sleep(500 * time.Millisecond)
	return instA, instB, legURL, inboundID, mock
}

func forLeg(id string) func(events.Event) bool {
	return func(e events.Event) bool { return e.Data.GetLegID() == id }
}

// The speaking detector needs 300ms of silence; the rest is jitter and the
// leg's outbound queue. Without the fix the agent kept talking for seconds.
const agentCutoffBudget = 1500 * time.Millisecond

func assertAgentSilencedWithin(t *testing.T, instB *testInstance, inboundID string, since time.Time) {
	t.Helper()
	instB.collector.waitForMatch(t, events.SpeakingStopped, forLeg(inboundID), agentCutoffBudget)
	t.Logf("far end went silent %v after the cut", time.Since(since).Round(10*time.Millisecond))

	started := len(instB.collector.matchAll(events.SpeakingStarted, forLeg(inboundID)))
	time.Sleep(time.Second)
	if n := len(instB.collector.matchAll(events.SpeakingStarted, forLeg(inboundID))); n != started {
		t.Fatalf("agent speech resumed after the cut (%d new speaking.started)", n-started)
	}
}

// TestAgent_LegDetachDiscardsBufferedSpeech: DELETE /agent on a standalone leg
// must stop the agent's buffered speech before it returns, so audio played
// next does not interleave with it.
func TestAgent_LegDetachDiscardsBufferedSpeech(t *testing.T) {
	_, instB, legURL, inboundID, _ := startBurstAgentCall(t, "agent-detach")

	t0 := time.Now()
	resp := httpDelete(t, legURL+"/agent")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("detach agent: status %d", resp.StatusCode)
	}
	assertAgentSilencedWithin(t, instB, inboundID, t0)
}

// TestAgent_LegInterruptionDiscardsBufferedSpeech: an interruption from the
// provider must cut the agent's buffered speech while the agent stays attached.
func TestAgent_LegInterruptionDiscardsBufferedSpeech(t *testing.T) {
	_, instB, legURL, inboundID, mock := startBurstAgentCall(t, "agent-barge")

	t0 := time.Now()
	mock.interrupt(t)
	assertAgentSilencedWithin(t, instB, inboundID, t0)

	resp := httpDelete(t, legURL+"/agent")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("agent should still be attached after interruption: status %d", resp.StatusCode)
	}
}
