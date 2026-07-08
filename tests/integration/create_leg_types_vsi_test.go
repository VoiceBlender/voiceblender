//go:build integration

package integration

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/wsmedia"
)

// TestVSI_CreateLeg_WebSocket originates an outbound WebSocket leg over the VSI
// WebSocket (create_leg type "websocket") against an in-test echo server and
// confirms it reaches connected.
func TestVSI_CreateLeg_WebSocket(t *testing.T) {
	inst := newTestInstance(t, "vsi-createleg-ws")

	srvCfg := wsmedia.Config{SampleRate: 16000, WireFormat: wsmedia.WireBinary, Log: slog.Default()}
	if err := srvCfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		c := srvCfg
		c.Log = slog.Default()
		tr, _, err := wsmedia.UpgradeServer(w, r, c)
		if err != nil {
			return
		}
		// Audio loopback so the server transport has a valid reader to send.
		pr, pw := io.Pipe()
		go func() {
			defer pw.Close()
			ar := tr.AudioReader()
			buf := make([]byte, c.FrameBytesPCM())
			for {
				if _, err := io.ReadFull(ar, buf); err != nil {
					return
				}
				if _, err := pw.Write(buf); err != nil {
					return
				}
			}
		}()
		tr.Start(pr)
		<-tr.Done()
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	conn := dialVSI(t, inst)
	defer conn.Close()
	readWSFrame(t, conn, 5*time.Second)

	f := vsiSend(t, conn, "create_leg", "ws-1", map[string]any{
		"type":        "websocket",
		"url":         wsURL,
		"sample_rate": 16000,
		"wire_format": "binary",
	})
	if f.Type != "create_leg.result" {
		t.Fatalf("type = %q, want create_leg.result (data=%s)", f.Type, f.Data)
	}
	var v legView
	if err := json.Unmarshal(f.Data, &v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v.ID == "" {
		t.Fatal("empty leg id")
	}
	inst.collector.waitForMatch(t, events.LegConnected, func(e events.Event) bool {
		return e.Data.GetLegID() == v.ID
	}, 3*time.Second)
}

// TestVSI_CreateLeg_WebSocketValidation confirms a missing url surfaces a VSI
// error frame (400), not a silent accept.
func TestVSI_CreateLeg_WebSocketValidation(t *testing.T) {
	inst := newTestInstance(t, "vsi-createleg-ws-bad")
	conn := dialVSI(t, inst)
	defer conn.Close()
	readWSFrame(t, conn, 5*time.Second)

	f := vsiSend(t, conn, "create_leg", "ws-bad", map[string]any{"type": "websocket"})
	assertVSIErrorCode(t, f, 400)
}

// TestVSI_CreateLeg_WhatsAppValidation confirms whatsapp validation runs over
// VSI (missing 'to' → 400) rather than the old "unsupported leg type" error.
func TestVSI_CreateLeg_WhatsAppValidation(t *testing.T) {
	inst := newTestInstance(t, "vsi-createleg-wa-bad")
	conn := dialVSI(t, inst)
	defer conn.Close()
	readWSFrame(t, conn, 5*time.Second)

	f := vsiSend(t, conn, "create_leg", "wa-bad", map[string]any{"type": "whatsapp"})
	assertVSIErrorCode(t, f, 400)
}

// TestVSI_CreateLeg_LiveKitError confirms livekit_room is dispatched over VSI
// and surfaces a real error frame (503 when LiveKit is disabled — the default
// test config — or 400 for missing params when enabled), never a 501/unsupported.
func TestVSI_CreateLeg_LiveKitError(t *testing.T) {
	inst := newTestInstance(t, "vsi-createleg-lk-bad")
	conn := dialVSI(t, inst)
	defer conn.Close()
	readWSFrame(t, conn, 5*time.Second)

	f := vsiSend(t, conn, "create_leg", "lk-bad", map[string]any{"type": "livekit_room"})
	if f.Type != "error" {
		t.Fatalf("type = %q, want error (data=%s)", f.Type, f.Data)
	}
	var e struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(f.Data, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != 400 && e.Code != 503 {
		t.Errorf("code = %d, want 400 or 503", e.Code)
	}
}

func assertVSIErrorCode(t *testing.T, f vsiResultFrame, want int) {
	t.Helper()
	if f.Type != "error" {
		t.Fatalf("type = %q, want error (data=%s)", f.Type, f.Data)
	}
	var e struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(f.Data, &e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != want {
		t.Errorf("code = %d, want %d", e.Code, want)
	}
}
