//go:build integration

package integration

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/wsmedia"
	"github.com/VoiceBlender/voiceblender/internal/wsutilx"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

// wsLegView is a local copy of api.LegView with the new `headers` field
// that the integration suite's legView (call_test.go) doesn't yet expose.
type wsLegView struct {
	ID         string            `json:"id"`
	Type       string            `json:"type"`
	State      string            `json:"state"`
	RoomID     string            `json:"room_id,omitempty"`
	Muted      bool              `json:"muted"`
	Deaf       bool              `json:"deaf"`
	Held       bool              `json:"held"`
	SIPHeaders map[string]string `json:"sip_headers,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
}

// ----- inbound: WebSocket leg auto-connects, joins room, exchanges audio -----

func TestWSLegInboundAutoConnect(t *testing.T) {
	inst := newTestInstance(t, "ws-inbound")

	wsURL := "ws://" + inst.httpAddr +
		"/v1/legs/websocket?sample_rate=16000&wire_format=binary&room_id=ws-room&rtt=true"

	dialCfg := ws.Dialer{
		Header: ws.HandshakeHeaderHTTP(http.Header{
			"X-Tenant":              []string{"tenant-a"},
			"P-Asserted-Identity":   []string{"alice@example.com"},
			"X-Boring-Other-Header": []string{""},
		}),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, _, err := dialCfg.Dial(ctx, wsURL)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Wait for both ringing and connected events.
	ringing := inst.collector.waitForMatch(t, events.LegRinging, nil, 2*time.Second)
	legID := ringing.Data.GetLegID()
	inst.collector.waitForMatch(t, events.LegConnected, func(e events.Event) bool {
		return e.Data.GetLegID() == legID
	}, 2*time.Second)

	// Send one 16kHz / 20ms PCM frame as binary.
	frame := make([]byte, 640)
	for i := 0; i < 320; i++ {
		binary.LittleEndian.PutUint16(frame[i*2:], uint16(int16(i*200)))
	}
	if err := wsutil.WriteClientBinary(conn, frame); err != nil {
		t.Fatalf("write audio: %v", err)
	}

	// Send a text message and expect rtt.received.
	if err := wsutil.WriteClientText(conn, []byte(`{"type":"text","text":"hello"}`)); err != nil {
		t.Fatalf("write text: %v", err)
	}
	inst.collector.waitForMatch(t, events.RTTReceived, func(e events.Event) bool {
		d, ok := e.Data.(*events.RTTReceivedData)
		return ok && d.LegID == legID && d.Text == "hello"
	}, 2*time.Second)

	// Verify headers exposed via LegView.
	resp := httpGet(t, inst.baseURL()+"/v1/legs/"+legID)
	var view wsLegView
	decodeJSON(t, resp, &view)
	if view.Headers["X-Tenant"] != "tenant-a" {
		t.Fatalf("X-Tenant missing/wrong in headers: %#v", view.Headers)
	}
	if view.Headers["P-Asserted-Identity"] != "alice@example.com" {
		t.Fatalf("P-Asserted-Identity missing/wrong: %#v", view.Headers)
	}
	if _, ok := view.Headers["User-Agent"]; ok {
		t.Fatalf("non-X-/P- header leaked through: %#v", view.Headers)
	}

	// Hang up via API.
	delResp := httpDelete(t, inst.baseURL()+"/v1/legs/"+legID)
	if delResp.StatusCode != http.StatusAccepted && delResp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE status=%d", delResp.StatusCode)
	}
	inst.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == legID
	}, 3*time.Second)
}

// ----- outbound: VB dials a remote WS, exchanges audio + headers ------------

func TestWSLegOutboundDialAndHeaders(t *testing.T) {
	inst := newTestInstance(t, "ws-outbound")

	// In-test echo server (acts as the "remote agent").
	var headerSeen sync.Map
	srvCfg := wsmedia.Config{SampleRate: 16000, WireFormat: wsmedia.WireBinary, Log: slog.Default()}
	if err := srvCfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		for k, v := range r.Header {
			headerSeen.Store(k, v)
		}
		c := srvCfg
		c.Log = slog.Default()
		tr, _, err := wsmedia.UpgradeServer(w, r, c)
		if err != nil {
			return
		}
		// Audio loopback.
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

	createResp := httpPost(t, inst.baseURL()+"/v1/legs", map[string]any{
		"type":        "websocket",
		"url":         wsURL,
		"sample_rate": 16000,
		"wire_format": "binary",
		"headers":     map[string]string{"X-Correlation-ID": "abc"},
		"room_id":     "out-room",
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /legs status=%d", createResp.StatusCode)
	}
	var created wsLegView
	decodeJSON(t, createResp, &created)
	legID := created.ID

	inst.collector.waitForMatch(t, events.LegRinging, func(e events.Event) bool {
		return e.Data.GetLegID() == legID
	}, 2*time.Second)
	inst.collector.waitForMatch(t, events.LegConnected, func(e events.Event) bool {
		return e.Data.GetLegID() == legID
	}, 3*time.Second)

	// Echo server saw X-Correlation-ID.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if v, ok := headerSeen.Load("X-Correlation-Id"); ok {
			vs := v.([]string)
			if len(vs) > 0 && vs[0] == "abc" {
				goto seen
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("server never saw X-Correlation-ID")
seen:

	// Hang up.
	delResp := httpDelete(t, inst.baseURL()+"/v1/legs/"+legID)
	if delResp.StatusCode != http.StatusAccepted && delResp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE status=%d", delResp.StatusCode)
	}
	inst.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == legID
	}, 3*time.Second)
}

// ----- outbound dial failure → disconnect with mapped reason -----------------

func TestWSLegOutboundDialFailure(t *testing.T) {
	inst := newTestInstance(t, "ws-outbound-fail")

	createResp := httpPost(t, inst.baseURL()+"/v1/legs", map[string]any{
		"type":         "websocket",
		"url":          "ws://127.0.0.1:1/", // port 1 nothing listens
		"sample_rate":  16000,
		"wire_format":  "binary",
		"ring_timeout": 1,
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /legs status=%d", createResp.StatusCode)
	}
	var created wsLegView
	decodeJSON(t, createResp, &created)
	legID := created.ID

	inst.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == legID
	}, 4*time.Second)
}

// Stuck-peer write-deadline detection is exercised by the wsmedia unit tests;
// integration-level testing on localhost is unreliable because the kernel TCP
// buffers are large enough that the write deadline rarely trips within a
// reasonable test budget.

// quick echo control test confirming pong replies and text payloads survive.
func TestWSLegPing(t *testing.T) {
	inst := newTestInstance(t, "ws-ping")

	wsURL := "ws://" + inst.httpAddr +
		"/v1/legs/websocket?sample_rate=16000&wire_format=binary"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, _, err := ws.Dial(ctx, wsURL)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	ringing := inst.collector.waitForMatch(t, events.LegRinging, nil, 2*time.Second)
	legID := ringing.Data.GetLegID()

	// Send a control ping and confirm we get a pong back.
	pingMsg, _ := json.Marshal(map[string]any{"type": "ping", "event_id": 7})
	if err := wsutil.WriteClientText(conn, pingMsg); err != nil {
		t.Fatalf("write ping: %v", err)
	}

	wsutilx.SetReadDeadline(conn, 2*time.Second)
	hdr, err := ws.ReadHeader(conn)
	if err != nil {
		t.Fatalf("read header: %v", err)
	}
	payload := make([]byte, hdr.Length)
	if _, err := io.ReadFull(conn, payload); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if hdr.Masked {
		ws.Cipher(payload, hdr.Mask, 0)
	}
	var got map[string]any
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("unmarshal pong: %v", err)
	}
	if got["type"] != "pong" {
		t.Fatalf("want pong, got %v", got)
	}
	if id, _ := got["event_id"].(float64); int(id) != 7 {
		t.Fatalf("event_id mismatch: %v", got["event_id"])
	}

	_ = fmt.Sprintf("%s", legID) // silence unused if test trims later
	httpDelete(t, inst.baseURL()+"/v1/legs/"+legID)
}
