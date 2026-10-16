//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/providercfg"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

// vendorOriginMock stands in for a regional Deepgram/ElevenLabs origin and
// records every request URI, plus the first text frame of each WebSocket.
type vendorOriginMock struct {
	url string

	mu         sync.Mutex
	uris       []string
	firstFrame map[string]string
}

func newVendorOriginMock(t *testing.T) *vendorOriginMock {
	t.Helper()
	m := &vendorOriginMock{firstFrame: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uri := r.URL.RequestURI()
		m.mu.Lock()
		m.uris = append(m.uris, uri)
		m.mu.Unlock()

		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			w.Header().Set("Content-Type", "audio/pcm")
			_, _ = w.Write(make([]byte, 3200))
			return
		}
		conn, _, _, err := ws.UpgradeHTTP(r, w)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			data, op, err := wsutil.ReadClientData(conn)
			if err != nil {
				return
			}
			if op != ws.OpText {
				continue
			}
			m.mu.Lock()
			if _, ok := m.firstFrame[r.URL.Path]; !ok {
				m.firstFrame[r.URL.Path] = string(data)
			}
			m.mu.Unlock()
		}
	}))
	t.Cleanup(srv.Close)
	m.url = srv.URL
	return m
}

func (m *vendorOriginMock) uriFor(path string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, u := range m.uris {
		if strings.HasPrefix(u, path+"?") || u == path {
			return u
		}
	}
	return ""
}

func (m *vendorOriginMock) frameFor(path string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.firstFrame[path]
}

// TestVendorDataHandling_RegionAndRetentionFlags drives each Deepgram and
// ElevenLabs integration through the REST API with the base URLs pointed at a
// mock origin, and checks the opt-out flags reach the wire.
func TestVendorDataHandling_RegionAndRetentionFlags(t *testing.T) {
	mock := newVendorOriginMock(t)

	instA := newTestInstanceWithOpts(t, "vendor-cfg-a", func(c *config.Config) {
		c.DeepgramAPIKey = "dg-test-key"
		c.ElevenLabsAPIKey = "el-test-key"
		c.Deepgram = providercfg.Deepgram{BaseURL: mock.url, MIPOptOut: true}
		c.ElevenLabs = providercfg.ElevenLabs{BaseURL: mock.url, DisableLogging: true}
	})
	instB := newTestInstance(t, "vendor-cfg-b")
	legID, _ := establishCall(t, instA, instB)
	legURL := fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), legID)

	post := func(path string, body map[string]any) {
		t.Helper()
		resp := httpPost(t, legURL+path, body)
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			t.Fatalf("POST %s: status %d", path, resp.StatusCode)
		}
	}
	stopSTT := func() {
		t.Helper()
		httpDelete(t, legURL+"/stt").Body.Close()
	}
	waitURI := func(path string) string {
		t.Helper()
		waitForCond(t, 10*time.Second, "request to "+path, func() bool { return mock.uriFor(path) != "" })
		return mock.uriFor(path)
	}

	post("/stt", map[string]any{"provider": "deepgram"})
	if u := waitURI("/v1/listen"); !strings.Contains(u, "mip_opt_out=true") {
		t.Errorf("deepgram v1 STT URI %q lacks mip_opt_out=true", u)
	}
	stopSTT()

	post("/stt", map[string]any{"provider": "deepgram_flux"})
	if u := waitURI("/v2/listen"); !strings.Contains(u, "mip_opt_out=true") {
		t.Errorf("deepgram flux URI %q lacks mip_opt_out=true", u)
	}
	stopSTT()

	post("/stt", map[string]any{"provider": "elevenlabs"})
	if u := waitURI("/v1/speech-to-text/realtime"); !strings.Contains(u, "enable_logging=false") {
		t.Errorf("elevenlabs STT URI %q lacks enable_logging=false", u)
	}
	stopSTT()

	post("/tts", map[string]any{"provider": "deepgram", "text": "hello", "voice": "aura-2-asteria-en"})
	if u := waitURI("/v1/speak"); !strings.Contains(u, "mip_opt_out=true") {
		t.Errorf("deepgram TTS URI %q lacks mip_opt_out=true", u)
	}

	post("/agent/deepgram", map[string]any{})
	waitURI("/v1/agent/converse")
	waitForCond(t, 10*time.Second, "agent Settings frame", func() bool { return mock.frameFor("/v1/agent/converse") != "" })
	var settings map[string]any
	if err := json.Unmarshal([]byte(mock.frameFor("/v1/agent/converse")), &settings); err != nil {
		t.Fatalf("decode Settings: %v", err)
	}
	if settings["mip_opt_out"] != true {
		t.Errorf("Settings.mip_opt_out = %v, want true", settings["mip_opt_out"])
	}
	httpDelete(t, legURL+"/agent").Body.Close()

	post("/agent/elevenlabs", map[string]any{"agent_id": "agent-123"})
	if u := waitURI("/v1/convai/conversation"); !strings.Contains(u, "agent_id=agent-123") {
		t.Errorf("elevenlabs agent URI = %q", u)
	}
	httpDelete(t, legURL+"/agent").Body.Close()
}
