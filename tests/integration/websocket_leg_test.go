//go:build integration

package integration

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
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

// TestWSLegAudioFlows verifies that audio actually traverses the full
// pipeline: a tone is played into a room, the WS leg in that room receives
// it from the mixer, encodes it as binary PCM, and ships it over the WS.
// We measure the RMS of the received frames to confirm the bytes are real
// audio (not silence or zero-padding).
func TestWSLegAudioFlows(t *testing.T) {
	inst := newTestInstance(t, "ws-audio-flow")

	wsURL := "ws://" + inst.httpAddr +
		"/v1/legs/websocket?sample_rate=16000&wire_format=binary&room_id=audio-room"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, _, err := ws.Dial(ctx, wsURL)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Wait for the leg to be registered + added to the room before we kick
	// off playback (otherwise the tone player has no participants to feed).
	ringing := inst.collector.waitForMatch(t, events.LegRinging, nil, 2*time.Second)
	legID := ringing.Data.GetLegID()
	inst.collector.waitForMatch(t, events.LegConnected, func(e events.Event) bool {
		return e.Data.GetLegID() == legID
	}, 2*time.Second)
	inst.collector.waitForMatch(t, events.LegJoinedRoom, func(e events.Event) bool {
		return e.Data.GetLegID() == legID
	}, 2*time.Second)

	// Start a looping dial-tone playback into the room.
	playResp := httpPost(t, inst.baseURL()+"/v1/rooms/audio-room/play", map[string]any{
		"tone":   "us_dial",
		"repeat": -1,
	})
	if playResp.StatusCode != http.StatusOK {
		t.Fatalf("play: status=%d", playResp.StatusCode)
	}
	playResp.Body.Close()

	// Read binary frames off the WS until we either:
	//  - accumulate enough audio to compute a meaningful RMS, or
	//  - run out of time.
	const (
		frameBytes = 640 // 16kHz × 20ms × 2 bytes/sample
		minFrames  = 25  // ~500 ms of audio
		readBudget = 5 * time.Second
	)

	var (
		audioBytes []byte
		gotFrames  int
		deadline   = time.Now().Add(readBudget)
	)
	for gotFrames < minFrames && time.Now().Before(deadline) {
		wsutilx.SetReadDeadline(conn, time.Until(deadline))
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
		// Skip control + non-binary frames — only audio counts.
		if hdr.OpCode != ws.OpBinary {
			continue
		}
		if len(payload) != frameBytes {
			t.Fatalf("unexpected binary frame size: got %d, want %d", len(payload), frameBytes)
		}
		audioBytes = append(audioBytes, payload...)
		gotFrames++
	}

	if gotFrames < minFrames {
		t.Fatalf("only got %d audio frames within %v (want %d)", gotFrames, readBudget, minFrames)
	}

	// Compute RMS of the collected PCM. A continuous dial tone at
	// nominal level sits around RMS 8000-15000 in int16 space; pure
	// silence is 0. Use 200 as a generous floor to confirm the signal
	// is non-trivial.
	var sumSquares float64
	sampleCount := len(audioBytes) / 2
	for i := 0; i < sampleCount; i++ {
		s := int16(binary.LittleEndian.Uint16(audioBytes[i*2:]))
		sumSquares += float64(s) * float64(s)
	}
	rms := 0.0
	if sampleCount > 0 {
		rms = sqrt(sumSquares / float64(sampleCount))
	}
	t.Logf("WS leg received %d frames (%d samples), RMS=%.1f", gotFrames, sampleCount, rms)
	if rms < 200 {
		t.Fatalf("RMS=%.1f is too low; audio is silent or near-silent (want >200 for a dial tone)", rms)
	}

	httpDelete(t, inst.baseURL()+"/v1/legs/"+legID)
}

// TestWSLegAudioFlowsBidirectional verifies audio in both directions: two
// WebSocket legs sit in the same room; client A streams a synthesized 1 kHz
// sine wave, the mixer routes it (mixed-minus-self) to client B, and we
// confirm the bytes B receives carry real audio. Symmetric to
// TestWSLegAudioFlows, but exercises the ingress (WS → mixer) path that
// the egress-only test doesn't cover.
func TestWSLegAudioFlowsBidirectional(t *testing.T) {
	inst := newTestInstance(t, "ws-bidi-flow")

	wsURL := "ws://" + inst.httpAddr +
		"/v1/legs/websocket?sample_rate=16000&wire_format=binary&room_id=bidi-room"

	seen := map[string]bool{}
	dial := func(label string) (net.Conn, string) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn, _, _, err := ws.Dial(ctx, wsURL)
		if err != nil {
			t.Fatalf("dial %s: %v", label, err)
		}
		ringing := inst.collector.waitForMatch(t, events.LegRinging, func(e events.Event) bool {
			return !seen[e.Data.GetLegID()]
		}, 2*time.Second)
		legID := ringing.Data.GetLegID()
		seen[legID] = true
		inst.collector.waitForMatch(t, events.LegConnected, func(e events.Event) bool {
			return e.Data.GetLegID() == legID
		}, 2*time.Second)
		inst.collector.waitForMatch(t, events.LegJoinedRoom, func(e events.Event) bool {
			return e.Data.GetLegID() == legID
		}, 2*time.Second)
		return conn, legID
	}

	connA, legA := dial("A")
	defer connA.Close()
	connB, legB := dial("B")
	defer connB.Close()
	t.Logf("legA=%s legB=%s", legA, legB)

	// Client A streams a 1 kHz sine forever (until the test cancels).
	const (
		sampleRate = 16000
		frameBytes = 640 // 20ms @ 16kHz × 2 bytes/sample
		amplitude  = 8000.0
		freqHz     = 1000.0
	)
	stopSender := make(chan struct{})
	defer close(stopSender)
	go func() {
		var phase float64
		const dPhase = 2 * 3.141592653589793 * freqHz / sampleRate
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopSender:
				return
			case <-ticker.C:
				frame := make([]byte, frameBytes)
				for i := 0; i < frameBytes/2; i++ {
					sample := int16(amplitude * sineApprox(phase))
					binary.LittleEndian.PutUint16(frame[i*2:], uint16(sample))
					phase += dPhase
				}
				if err := wsutil.WriteClientBinary(connA, frame); err != nil {
					return
				}
			}
		}
	}()

	// Client B reads binary frames and computes RMS over ~500ms of audio.
	const (
		minFrames  = 25
		readBudget = 5 * time.Second
	)
	var (
		audioBytes []byte
		gotFrames  int
		deadline   = time.Now().Add(readBudget)
	)
	for gotFrames < minFrames && time.Now().Before(deadline) {
		wsutilx.SetReadDeadline(connB, time.Until(deadline))
		hdr, err := ws.ReadHeader(connB)
		if err != nil {
			t.Fatalf("B read header: %v", err)
		}
		payload := make([]byte, hdr.Length)
		if _, err := io.ReadFull(connB, payload); err != nil {
			t.Fatalf("B read payload: %v", err)
		}
		if hdr.Masked {
			ws.Cipher(payload, hdr.Mask, 0)
		}
		if hdr.OpCode != ws.OpBinary {
			continue
		}
		if len(payload) != frameBytes {
			t.Fatalf("unexpected B frame size: got %d, want %d", len(payload), frameBytes)
		}
		audioBytes = append(audioBytes, payload...)
		gotFrames++
	}

	if gotFrames < minFrames {
		t.Fatalf("B only got %d frames within %v (want %d)", gotFrames, readBudget, minFrames)
	}

	var sumSquares float64
	sampleCount := len(audioBytes) / 2
	for i := 0; i < sampleCount; i++ {
		s := int16(binary.LittleEndian.Uint16(audioBytes[i*2:]))
		sumSquares += float64(s) * float64(s)
	}
	rms := 0.0
	if sampleCount > 0 {
		rms = sqrt(sumSquares / float64(sampleCount))
	}
	t.Logf("legB received %d frames (%d samples), RMS=%.1f", gotFrames, sampleCount, rms)
	if rms < 200 {
		t.Fatalf("B RMS=%.1f is too low; ingress→mixer→egress audio path looks broken (want >200)", rms)
	}

	httpDelete(t, inst.baseURL()+"/v1/legs/"+legA)
	httpDelete(t, inst.baseURL()+"/v1/legs/"+legB)
}

// sineApprox is a tiny sine approximation good enough for an RMS check
// without pulling math into this build.
func sineApprox(x float64) float64 {
	// Wrap x into [-pi, pi].
	const twoPi = 2 * 3.141592653589793
	const pi = 3.141592653589793
	for x > pi {
		x -= twoPi
	}
	for x < -pi {
		x += twoPi
	}
	// Bhaskara I sine approximation — max error ~1.6e-3, plenty for an
	// RMS-above-floor test.
	return (16 * x * (pi - absF(x))) / (5*pi*pi - 4*absF(x)*(pi-absF(x)))
}

func absF(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// sqrt is a local helper to avoid pulling math into this test file.
func sqrt(x float64) float64 {
	if x <= 0 {
		return 0
	}
	z := x
	for i := 0; i < 16; i++ {
		z = (z + x/z) / 2
	}
	return z
}

// TestRoomWSCompatibleWithLegWS verifies the room-WS endpoint
// (/v1/rooms/{id}/ws) speaks the same wire protocol as /v1/legs/websocket
// (now that both share wsmedia.Transport): a leg WS client and a room WS
// client in the same room exchange audio in both directions using the
// JSON-base64 frame shape (`{"audio":"<b64>"}`).
func TestRoomWSCompatibleWithLegWS(t *testing.T) {
	inst := newTestInstance(t, "room-ws-compat")

	// Pre-create the room so /v1/rooms/{id}/ws finds it.
	createResp := httpPost(t, inst.baseURL()+"/v1/rooms", map[string]any{
		"id":          "compat-room",
		"sample_rate": 16000,
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create room: status=%d", createResp.StatusCode)
	}
	createResp.Body.Close()

	// Leg WS client (sender) — uses json_base64 to match room WS shape.
	legURL := "ws://" + inst.httpAddr +
		"/v1/legs/websocket?sample_rate=16000&wire_format=json_base64&room_id=compat-room"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	legConn, _, _, err := ws.Dial(ctx, legURL)
	if err != nil {
		t.Fatalf("dial leg WS: %v", err)
	}
	defer legConn.Close()

	ringing := inst.collector.waitForMatch(t, events.LegRinging, nil, 2*time.Second)
	legID := ringing.Data.GetLegID()
	inst.collector.waitForMatch(t, events.LegConnected, func(e events.Event) bool {
		return e.Data.GetLegID() == legID
	}, 2*time.Second)
	inst.collector.waitForMatch(t, events.LegJoinedRoom, func(e events.Event) bool {
		return e.Data.GetLegID() == legID
	}, 2*time.Second)

	// Room WS client (listener).
	roomURL := "ws://" + inst.httpAddr + "/v1/rooms/compat-room/ws"
	roomConn, _, _, err := ws.Dial(ctx, roomURL)
	if err != nil {
		t.Fatalf("dial room WS: %v", err)
	}
	defer roomConn.Close()

	// First text frame from room WS must be the welcome message.
	wsutilx.SetReadDeadline(roomConn, 3*time.Second)
	hdr, err := ws.ReadHeader(roomConn)
	if err != nil {
		t.Fatalf("read room welcome header: %v", err)
	}
	welcomeBytes := make([]byte, hdr.Length)
	if _, err := io.ReadFull(roomConn, welcomeBytes); err != nil {
		t.Fatalf("read welcome payload: %v", err)
	}
	if hdr.Masked {
		ws.Cipher(welcomeBytes, hdr.Mask, 0)
	}
	if hdr.OpCode != ws.OpText {
		t.Fatalf("welcome should be a text frame, got opcode %v", hdr.OpCode)
	}
	var welcome map[string]any
	if err := json.Unmarshal(welcomeBytes, &welcome); err != nil {
		t.Fatalf("unmarshal welcome: %v", err)
	}
	if welcome["type"] != "connected" {
		t.Fatalf("welcome.type = %v, want connected (raw=%s)", welcome["type"], welcomeBytes)
	}
	if welcome["format"] != "pcm_s16le" {
		t.Fatalf("welcome.format = %v, want pcm_s16le", welcome["format"])
	}
	if welcome["participant_id"] == nil {
		t.Fatalf("welcome missing participant_id (raw=%s)", welcomeBytes)
	}

	// Sender goroutine: leg WS streams a 1 kHz sine using the room-WS-style
	// JSON shape `{"audio":"<b64>"}` (no `type` field). The wsmedia
	// transport on the server side must accept this shape.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		var phase float64
		const dPhase = 2 * 3.141592653589793 * 1000.0 / 16000.0
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				pcm := make([]byte, 640)
				for i := 0; i < 320; i++ {
					sample := int16(8000 * sineApprox(phase))
					binary.LittleEndian.PutUint16(pcm[i*2:], uint16(sample))
					phase += dPhase
				}
				frame, _ := json.Marshal(map[string]string{
					"audio": base64.StdEncoding.EncodeToString(pcm),
				})
				if err := wsutil.WriteClientText(legConn, frame); err != nil {
					return
				}
			}
		}
	}()

	// Listener: read room WS frames, decode the room-WS shape, compute RMS.
	var (
		audioBytes []byte
		gotFrames  int
		deadline   = time.Now().Add(5 * time.Second)
	)
	for gotFrames < 25 && time.Now().Before(deadline) {
		wsutilx.SetReadDeadline(roomConn, time.Until(deadline))
		hdr, err := ws.ReadHeader(roomConn)
		if err != nil {
			t.Fatalf("room WS read header: %v", err)
		}
		payload := make([]byte, hdr.Length)
		if _, err := io.ReadFull(roomConn, payload); err != nil {
			t.Fatalf("room WS read payload: %v", err)
		}
		if hdr.Masked {
			ws.Cipher(payload, hdr.Mask, 0)
		}
		if hdr.OpCode != ws.OpText {
			continue
		}
		var msg map[string]any
		if err := json.Unmarshal(payload, &msg); err != nil {
			continue
		}
		// Skip control / heartbeat / welcome — only audio counts.
		audioStr, _ := msg["audio"].(string)
		if audioStr == "" {
			continue
		}
		pcm, err := base64.StdEncoding.DecodeString(audioStr)
		if err != nil {
			t.Fatalf("decode b64 audio: %v", err)
		}
		audioBytes = append(audioBytes, pcm...)
		gotFrames++
	}

	if gotFrames < 25 {
		t.Fatalf("room WS only got %d audio frames within 5s (want 25)", gotFrames)
	}
	var sumSquares float64
	sampleCount := len(audioBytes) / 2
	for i := 0; i < sampleCount; i++ {
		s := int16(binary.LittleEndian.Uint16(audioBytes[i*2:]))
		sumSquares += float64(s) * float64(s)
	}
	rms := sqrt(sumSquares / float64(sampleCount))
	t.Logf("room WS received %d frames (%d samples), RMS=%.1f", gotFrames, sampleCount, rms)
	if rms < 200 {
		t.Fatalf("RMS=%.1f too low — leg WS → mixer → room WS path looks broken", rms)
	}

	// Client-initiated close from the room WS using the room-style verb.
	stopFrame, _ := json.Marshal(map[string]string{"type": "stop"})
	if err := wsutil.WriteClientText(roomConn, stopFrame); err != nil {
		t.Fatalf("send stop: %v", err)
	}
	httpDelete(t, inst.baseURL()+"/v1/legs/"+legID)
}

// readJSONFrame reads the next text frame off conn, decodes the payload
// as a JSON object, and returns it.
func readJSONFrame(t *testing.T, conn net.Conn, timeout time.Duration) map[string]any {
	t.Helper()
	wsutilx.SetReadDeadline(conn, timeout)
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
	if hdr.OpCode != ws.OpText {
		t.Fatalf("expected text frame, got opcode %v", hdr.OpCode)
	}
	var out map[string]any
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatalf("unmarshal: %v (raw=%s)", err, payload)
	}
	return out
}

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

	// Skip the welcome `connected` frame the leg endpoint sends on upgrade.
	if got := readJSONFrame(t, conn, 2*time.Second); got["type"] != "connected" {
		t.Fatalf("first frame should be connected, got %v", got)
	}

	// Send a control ping and confirm we get a pong back.
	pingMsg, _ := json.Marshal(map[string]any{"type": "ping", "event_id": 7})
	if err := wsutil.WriteClientText(conn, pingMsg); err != nil {
		t.Fatalf("write ping: %v", err)
	}
	got := readJSONFrame(t, conn, 2*time.Second)
	if got["type"] != "pong" {
		t.Fatalf("want pong, got %v", got)
	}
	if id, _ := got["event_id"].(float64); int(id) != 7 {
		t.Fatalf("event_id mismatch: %v", got["event_id"])
	}

	_ = fmt.Sprintf("%s", legID) // silence unused if test trims later
	httpDelete(t, inst.baseURL()+"/v1/legs/"+legID)
}
