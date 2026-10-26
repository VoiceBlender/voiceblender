//go:build integration

package integration

import (
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
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
	"google.golang.org/protobuf/encoding/protowire"
)

// burstPipecatMock is a Pipecat bot stand-in speaking upstream's protobuf
// serializer wire format, encoded by hand so it cannot drift with our
// vendored frames.proto. Like the ElevenLabs mock it bursts a long response
// faster than real time on connect.
type burstPipecatMock struct {
	url string

	mu   sync.Mutex
	conn net.Conn
}

func pipecatFrame(field protowire.Number, inner []byte) []byte {
	b := protowire.AppendTag(nil, field, protowire.BytesType)
	return protowire.AppendBytes(b, inner)
}

func pipecatAudioFrame(pcm []byte) []byte {
	inner := protowire.AppendTag(nil, 3, protowire.BytesType)
	inner = protowire.AppendBytes(inner, pcm)
	inner = protowire.AppendTag(inner, 4, protowire.VarintType)
	inner = protowire.AppendVarint(inner, 16000)
	inner = protowire.AppendTag(inner, 5, protowire.VarintType)
	inner = protowire.AppendVarint(inner, 1)
	return pipecatFrame(2, inner)
}

func pipecatRTVIFrame(json string) []byte {
	inner := protowire.AppendTag(nil, 1, protowire.BytesType)
	inner = protowire.AppendString(inner, json)
	return pipecatFrame(4, inner)
}

func pipecatInterruptionFrame() []byte {
	inner := protowire.AppendTag(nil, 1, protowire.VarintType)
	inner = protowire.AppendVarint(inner, 1)
	return pipecatFrame(5, inner)
}

const pipecatBotGreeting = "hello from the pipecat bot"

func newBurstPipecatMock(t *testing.T) *burstPipecatMock {
	t.Helper()
	m := &burstPipecatMock{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, _, err := ws.UpgradeHTTP(r, w)
		if err != nil {
			return
		}
		defer conn.Close()
		m.mu.Lock()
		m.conn = conn
		_ = wsutil.WriteServerBinary(conn, pipecatRTVIFrame(
			fmt.Sprintf(`{"label":"rtvi-ai","type":"bot-transcription","data":{"text":%q}}`, pipecatBotGreeting)))
		const chunk = 100 * time.Millisecond
		for i := 0; i < int(burstAgentSpeech/chunk); i++ {
			_ = wsutil.WriteServerBinary(conn, pipecatAudioFrame(toneChunk16k(i, chunk)))
		}
		m.mu.Unlock()
		for {
			if _, _, err := wsutil.ReadClientData(conn); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	m.url = "ws" + strings.TrimPrefix(srv.URL, "http")
	return m
}

func (m *burstPipecatMock) interrupt(t *testing.T) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.conn == nil {
		t.Fatal("pipecat mock not connected")
	}
	if err := wsutil.WriteServerBinary(m.conn, pipecatInterruptionFrame()); err != nil {
		t.Fatalf("send interruption: %v", err)
	}
}

// TestAgent_PipecatInterruptionDiscardsBufferedSpeech: a Pipecat
// InterruptionFrame must cut the bot's buffered speech while the agent stays
// attached, and an RTVI bot-transcription must reach agent.agent_response.
func TestAgent_PipecatInterruptionDiscardsBufferedSpeech(t *testing.T) {
	mock := newBurstPipecatMock(t)
	instA := newTestInstance(t, "pipecat-barge-a")
	instB := newTestInstanceWithOpts(t, "pipecat-barge-b", func(c *config.Config) { c.SpeechDetectionEnabled = true })
	outboundID, inboundID := establishCall(t, instA, instB)
	legURL := fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID)

	resp := httpPost(t, legURL+"/agent/pipecat", map[string]any{"websocket_url": mock.url})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("attach agent: status %d", resp.StatusCode)
	}

	instA.collector.waitForMatch(t, events.AgentAgentResponse, func(e events.Event) bool {
		d, ok := e.Data.(*events.AgentResponseData)
		return ok && d.LegID == outboundID && d.Text == pipecatBotGreeting
	}, 5*time.Second)

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
