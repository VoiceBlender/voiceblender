//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/csiwek/VoiceBlender/internal/api"
	"github.com/csiwek/VoiceBlender/internal/codec"
	"github.com/csiwek/VoiceBlender/internal/config"
	"github.com/csiwek/VoiceBlender/internal/events"
	"github.com/csiwek/VoiceBlender/internal/leg"
	"github.com/csiwek/VoiceBlender/internal/room"
	sipmod "github.com/csiwek/VoiceBlender/internal/sip"
	goaudio "github.com/go-audio/audio"
	"github.com/go-audio/wav"
)

// ---------------------------------------------------------------------------
// testInstance — encapsulates one full VoiceBlender stack
// ---------------------------------------------------------------------------

type testInstance struct {
	name     string
	cfg      config.Config
	bus      *events.Bus
	webhooks *events.WebhookRegistry
	legMgr   *leg.Manager
	roomMgr  *room.Manager
	engine   *sipmod.Engine
	apiSrv   *api.Server
	httpSrv  *http.Server
	httpAddr string // "127.0.0.1:<port>"
	sipPort  int
	collector *eventCollector
	cancel   context.CancelFunc
}

func newTestInstance(t *testing.T, name string) *testInstance {
	t.Helper()

	// Find a free UDP port for SIP.
	udpConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("[%s] find free UDP port: %v", name, err)
	}
	sipPort := udpConn.LocalAddr().(*net.UDPAddr).Port
	udpConn.Close()

	log := slog.Default().With("instance", name)

	recDir := t.TempDir()

	cfg := config.Config{
		SIPBindIP:    "127.0.0.1",
		SIPListenIP:  "127.0.0.1",
		SIPPort:      fmt.Sprintf("%d", sipPort),
		SIPHost:      name,
		HTTPAddr:     "127.0.0.1:0",
		RecordingDir: recDir,
	}

	bus := events.NewBus()
	webhooks := events.NewWebhookRegistry(bus, log)
	legMgr := leg.NewManager()
	roomMgr := room.NewManager(legMgr, bus, log)

	engine, err := sipmod.NewEngine(sipmod.EngineConfig{
		BindIP:   "127.0.0.1",
		ListenIP: "127.0.0.1",
		BindPort: sipPort,
		SIPHost:  name,
		Codecs:   []codec.CodecType{codec.CodecPCMU},
		Log:      log,
	})
	if err != nil {
		t.Fatalf("[%s] new engine: %v", name, err)
	}

	apiSrv := api.NewServer(legMgr, roomMgr, engine, bus, webhooks, nil, nil, cfg, log)
	engine.OnInvite(apiSrv.HandleInboundCall)

	ctx, cancel := context.WithCancel(context.Background())

	// Start SIP engine.
	go func() {
		if err := engine.Serve(ctx); err != nil && ctx.Err() == nil {
			log.Error("SIP engine error", "error", err)
		}
	}()

	// Start HTTP server on a dynamic port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		cancel()
		t.Fatalf("[%s] listen HTTP: %v", name, err)
	}
	httpAddr := ln.Addr().String()

	httpSrv := &http.Server{Handler: apiSrv}
	go func() {
		if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Error("HTTP server error", "error", err)
		}
	}()

	// Allow SIP engine to start listening.
	time.Sleep(200 * time.Millisecond)

	ec := newEventCollector()
	bus.Subscribe(ec.handle)

	inst := &testInstance{
		name:      name,
		cfg:       cfg,
		bus:       bus,
		webhooks:  webhooks,
		legMgr:    legMgr,
		roomMgr:   roomMgr,
		engine:    engine,
		apiSrv:    apiSrv,
		httpSrv:   httpSrv,
		httpAddr:  httpAddr,
		sipPort:   sipPort,
		collector: ec,
		cancel:    cancel,
	}

	t.Cleanup(func() {
		cancel()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer shutdownCancel()
		httpSrv.Shutdown(shutdownCtx)
		webhooks.Stop()
		// Hangup all remaining legs.
		for _, l := range legMgr.List() {
			l.Hangup(context.Background())
		}
	})

	return inst
}

func (inst *testInstance) baseURL() string {
	return "http://" + inst.httpAddr
}

// ---------------------------------------------------------------------------
// eventCollector — thread-safe event recorder
// ---------------------------------------------------------------------------

type eventCollector struct {
	mu     sync.Mutex
	events []events.Event
}

func newEventCollector() *eventCollector {
	return &eventCollector{}
}

func (ec *eventCollector) handle(e events.Event) {
	ec.mu.Lock()
	defer ec.mu.Unlock()
	ec.events = append(ec.events, e)
}

func (ec *eventCollector) waitForMatch(t *testing.T, typ events.EventType, pred func(events.Event) bool, timeout time.Duration) events.Event {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ec.mu.Lock()
		for _, e := range ec.events {
			if e.Type == typ && (pred == nil || pred(e)) {
				ec.mu.Unlock()
				return e
			}
		}
		ec.mu.Unlock()
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for event %s", typ)
	return events.Event{}
}

func (ec *eventCollector) hasEvent(typ events.EventType, pred func(events.Event) bool) bool {
	ec.mu.Lock()
	defer ec.mu.Unlock()
	for _, e := range ec.events {
		if e.Type == typ && (pred == nil || pred(e)) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// HTTP helpers
// ---------------------------------------------------------------------------

func httpPost(t *testing.T, url string, body interface{}) *http.Response {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

func httpGet(t *testing.T, url string) *http.Response {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp
}

func httpDelete(t *testing.T, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		t.Fatalf("new DELETE request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE %s: %v", url, err)
	}
	return resp
}

func decodeJSON(t *testing.T, resp *http.Response, v interface{}) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
}

type legView struct {
	ID     string `json:"leg_id"`
	Type   string `json:"type"`
	State  string `json:"state"`
	RoomID string `json:"room_id,omitempty"`
	Muted  bool   `json:"muted"`
}

type roomView struct {
	ID           string    `json:"id"`
	Participants []legView `json:"participants"`
}

type recordingResponse struct {
	Status string `json:"status"`
	File   string `json:"file"`
}

// assertWAVAudio opens a WAV file and verifies it is a valid WAV with the
// expected number of channels, 16-bit depth, and contains at least minSamples
// audio samples. Returns the total number of samples read.
func assertWAVAudio(t *testing.T, path string, wantChannels, wantSampleRate, minSamples int) int {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open WAV file %s: %v", path, err)
	}
	defer f.Close()

	dec := wav.NewDecoder(f)
	if !dec.IsValidFile() {
		t.Fatalf("%s is not a valid WAV file", path)
	}

	if int(dec.NumChans) != wantChannels {
		t.Fatalf("expected %d channel(s), got %d", wantChannels, dec.NumChans)
	}
	if int(dec.SampleRate) != wantSampleRate {
		t.Fatalf("expected sample rate %d, got %d", wantSampleRate, dec.SampleRate)
	}
	if int(dec.BitDepth) != 16 {
		t.Fatalf("expected 16-bit depth, got %d", dec.BitDepth)
	}

	buf := &goaudio.IntBuffer{Data: make([]int, 4096), Format: &goaudio.Format{
		SampleRate:  wantSampleRate,
		NumChannels: wantChannels,
	}}
	totalSamples := 0
	for {
		n, err := dec.PCMBuffer(buf)
		if n > 0 {
			totalSamples += n
		}
		if err != nil || n == 0 {
			break
		}
	}

	if totalSamples < minSamples {
		t.Fatalf("expected at least %d samples, got %d", minSamples, totalSamples)
	}
	t.Logf("WAV %s: %d ch, %d Hz, %d samples", path, wantChannels, wantSampleRate, totalSamples)
	return totalSamples
}

func waitForLegState(t *testing.T, baseURL, legID, wantState string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp := httpGet(t, fmt.Sprintf("%s/v1/legs/%s", baseURL, legID))
		var v legView
		decodeJSON(t, resp, &v)
		if v.State == wantState {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("leg %s did not reach state %q within %v", legID, wantState, timeout)
}

// waitForInboundLeg polls instance's leg list until a sip_inbound leg in ringing state appears.
func waitForInboundLeg(t *testing.T, baseURL string, timeout time.Duration) legView {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp := httpGet(t, baseURL+"/v1/legs")
		var legs []legView
		decodeJSON(t, resp, &legs)
		for _, l := range legs {
			if l.Type == "sip_inbound" && l.State == "ringing" {
				return l
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no inbound ringing leg found within %v", timeout)
	return legView{}
}

// ---------------------------------------------------------------------------
// Call setup helpers
// ---------------------------------------------------------------------------

// establishCall dials from A to B, answers on B, and waits for both legs to connect.
// Returns the outbound leg ID (on A) and inbound leg ID (on B).
func establishCall(t *testing.T, instA, instB *testInstance) (outboundID, inboundID string) {
	t.Helper()

	createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type":   "sip",
		"uri":    fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"codecs": []string{"PCMU"},
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: unexpected status %d", createResp.StatusCode)
	}
	var outbound legView
	decodeJSON(t, createResp, &outbound)

	inbound := waitForInboundLeg(t, instB.baseURL(), 5*time.Second)

	answerResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/answer", instB.baseURL(), inbound.ID), nil)
	if answerResp.StatusCode != http.StatusOK {
		t.Fatalf("answer: unexpected status %d", answerResp.StatusCode)
	}
	answerResp.Body.Close()

	waitForLegState(t, instA.baseURL(), outbound.ID, "connected", 5*time.Second)
	waitForLegState(t, instB.baseURL(), inbound.ID, "connected", 5*time.Second)

	return outbound.ID, inbound.ID
}

// ---------------------------------------------------------------------------
// Test Cases
// ---------------------------------------------------------------------------

func TestOutboundInbound_Connect(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")

	// A dials B.
	createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type":   "sip",
		"uri":    fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"codecs": []string{"PCMU"},
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: unexpected status %d", createResp.StatusCode)
	}
	var outboundLeg legView
	decodeJSON(t, createResp, &outboundLeg)
	t.Logf("outbound leg: %s", outboundLeg.ID)

	// Wait for inbound leg on B.
	inboundLeg := waitForInboundLeg(t, instB.baseURL(), 5*time.Second)
	t.Logf("inbound leg: %s", inboundLeg.ID)

	// B answers.
	answerResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/answer", instB.baseURL(), inboundLeg.ID), nil)
	if answerResp.StatusCode != http.StatusOK {
		t.Fatalf("answer leg: unexpected status %d", answerResp.StatusCode)
	}
	answerResp.Body.Close()

	// Verify both legs reach connected state.
	waitForLegState(t, instA.baseURL(), outboundLeg.ID, "connected", 5*time.Second)
	waitForLegState(t, instB.baseURL(), inboundLeg.ID, "connected", 5*time.Second)

	// Verify events on both sides.
	instA.collector.waitForMatch(t, events.LegRinging, nil, 1*time.Second)
	instA.collector.waitForMatch(t, events.LegConnected, nil, 1*time.Second)
	instB.collector.waitForMatch(t, events.LegRinging, nil, 1*time.Second)
	instB.collector.waitForMatch(t, events.LegConnected, nil, 1*time.Second)

	// A hangs up.
	delResp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundLeg.ID))
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("delete leg: unexpected status %d", delResp.StatusCode)
	}
	delResp.Body.Close()

	// Verify disconnected events on both sides.
	instA.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data["leg_id"] == outboundLeg.ID
	}, 5*time.Second)
	instB.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data["leg_id"] == inboundLeg.ID
	}, 5*time.Second)
}

func TestDisconnect_DurationFields(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	outboundID, inboundID := establishCall(t, instA, instB)

	// Let the call run briefly so durations are non-zero.
	time.Sleep(200 * time.Millisecond)

	// A hangs up.
	delResp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("delete leg: unexpected status %d", delResp.StatusCode)
	}
	delResp.Body.Close()

	// Verify outbound disconnect event has duration fields.
	eA := instA.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data["leg_id"] == outboundID
	}, 5*time.Second)

	dTotal, ok := eA.Data["duration_total"].(float64)
	if !ok {
		t.Fatalf("duration_total missing or not float64: %v", eA.Data["duration_total"])
	}
	if dTotal < 0.1 {
		t.Fatalf("expected duration_total >= 0.1s, got %f", dTotal)
	}

	dAnswered, ok := eA.Data["duration_answered"].(float64)
	if !ok {
		t.Fatalf("duration_answered missing or not float64: %v", eA.Data["duration_answered"])
	}
	if dAnswered < 0.1 {
		t.Fatalf("expected duration_answered >= 0.1s, got %f", dAnswered)
	}
	if dAnswered > dTotal {
		t.Fatalf("duration_answered (%f) > duration_total (%f)", dAnswered, dTotal)
	}

	t.Logf("outbound: duration_total=%.3fs duration_answered=%.3fs", dTotal, dAnswered)

	// Verify inbound disconnect event on B also has duration fields.
	eB := instB.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data["leg_id"] == inboundID
	}, 5*time.Second)

	dTotalB, ok := eB.Data["duration_total"].(float64)
	if !ok {
		t.Fatalf("B: duration_total missing or not float64: %v", eB.Data["duration_total"])
	}
	if dTotalB < 0.1 {
		t.Fatalf("B: expected duration_total >= 0.1s, got %f", dTotalB)
	}

	dAnsweredB, ok := eB.Data["duration_answered"].(float64)
	if !ok {
		t.Fatalf("B: duration_answered missing or not float64: %v", eB.Data["duration_answered"])
	}
	if dAnsweredB < 0.1 {
		t.Fatalf("B: expected duration_answered >= 0.1s, got %f", dAnsweredB)
	}

	t.Logf("inbound: duration_total=%.3fs duration_answered=%.3fs", dTotalB, dAnsweredB)
}

func TestDisconnect_UnansweredDuration(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")

	// A dials B with a short ring timeout.
	createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type":         "sip",
		"uri":          fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"codecs":       []string{"PCMU"},
		"ring_timeout": 1,
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: unexpected status %d", createResp.StatusCode)
	}
	var outboundLeg legView
	decodeJSON(t, createResp, &outboundLeg)

	// Don't answer. Wait for disconnect.
	e := instA.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data["leg_id"] == outboundLeg.ID
	}, 5*time.Second)

	// duration_total should be > 0 (at least the ring timeout).
	dTotal, ok := e.Data["duration_total"].(float64)
	if !ok {
		t.Fatalf("duration_total missing or not float64: %v", e.Data["duration_total"])
	}
	if dTotal < 0.5 {
		t.Fatalf("expected duration_total >= 0.5s, got %f", dTotal)
	}

	// duration_answered should be 0 (never answered).
	dAnswered, ok := e.Data["duration_answered"].(float64)
	if !ok {
		t.Fatalf("duration_answered missing or not float64: %v", e.Data["duration_answered"])
	}
	if dAnswered != 0 {
		t.Fatalf("expected duration_answered == 0, got %f", dAnswered)
	}

	t.Logf("unanswered: duration_total=%.3fs duration_answered=%.3fs", dTotal, dAnswered)
}

func TestOutboundInbound_CallerCancel(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")

	// A dials B.
	createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type":   "sip",
		"uri":    fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"codecs": []string{"PCMU"},
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: unexpected status %d", createResp.StatusCode)
	}
	var outboundLeg legView
	decodeJSON(t, createResp, &outboundLeg)

	// Wait for inbound leg on B.
	inboundLeg := waitForInboundLeg(t, instB.baseURL(), 5*time.Second)

	// A cancels (hangs up before B answers).
	delResp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundLeg.ID))
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("delete leg: unexpected status %d", delResp.StatusCode)
	}
	delResp.Body.Close()

	// Verify B sees leg.disconnected with reason "caller_cancel".
	instB.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data["leg_id"] == inboundLeg.ID && e.Data["reason"] == "caller_cancel"
	}, 5*time.Second)
}

func TestOutboundInbound_RoomBridge(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")

	// Establish call: A dials B, B answers.
	createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type":   "sip",
		"uri":    fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"codecs": []string{"PCMU"},
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

	// Create room on A.
	roomResp := httpPost(t, instA.baseURL()+"/v1/rooms", map[string]interface{}{})
	if roomResp.StatusCode != http.StatusCreated {
		t.Fatalf("create room: unexpected status %d", roomResp.StatusCode)
	}
	var rm roomView
	decodeJSON(t, roomResp, &rm)
	t.Logf("room: %s", rm.ID)

	// Add outbound leg to room.
	addResp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/legs", instA.baseURL(), rm.ID), map[string]interface{}{
		"leg_id": outboundLeg.ID,
	})
	if addResp.StatusCode != http.StatusOK {
		t.Fatalf("add leg to room: unexpected status %d", addResp.StatusCode)
	}
	addResp.Body.Close()

	// Verify leg.joined_room event.
	instA.collector.waitForMatch(t, events.LegJoinedRoom, func(e events.Event) bool {
		return e.Data["leg_id"] == outboundLeg.ID && e.Data["room_id"] == rm.ID
	}, 3*time.Second)

	// Verify GET /v1/rooms/{id} shows the participant.
	getRoomResp := httpGet(t, fmt.Sprintf("%s/v1/rooms/%s", instA.baseURL(), rm.ID))
	var gotRoom roomView
	decodeJSON(t, getRoomResp, &gotRoom)
	if len(gotRoom.Participants) != 1 {
		t.Fatalf("expected 1 participant, got %d", len(gotRoom.Participants))
	}
	if gotRoom.Participants[0].ID != outboundLeg.ID {
		t.Fatalf("expected participant %s, got %s", outboundLeg.ID, gotRoom.Participants[0].ID)
	}

	// Remove leg from room.
	removeResp := httpDelete(t, fmt.Sprintf("%s/v1/rooms/%s/legs/%s", instA.baseURL(), rm.ID, outboundLeg.ID))
	if removeResp.StatusCode != http.StatusOK {
		t.Fatalf("remove leg from room: unexpected status %d", removeResp.StatusCode)
	}
	removeResp.Body.Close()

	// Verify leg.left_room event.
	instA.collector.waitForMatch(t, events.LegLeftRoom, func(e events.Event) bool {
		return e.Data["leg_id"] == outboundLeg.ID && e.Data["room_id"] == rm.ID
	}, 3*time.Second)

	// Cleanup: hangup.
	delResp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundLeg.ID))
	delResp.Body.Close()
}

func TestOutboundInbound_RingTimeout(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")

	// A dials B with a short ring timeout.
	createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type":         "sip",
		"uri":          fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"codecs":       []string{"PCMU"},
		"ring_timeout": 2,
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: unexpected status %d", createResp.StatusCode)
	}
	var outboundLeg legView
	decodeJSON(t, createResp, &outboundLeg)

	// Do NOT answer on B. Wait for A to see leg.disconnected.
	instA.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data["leg_id"] == outboundLeg.ID
	}, 5*time.Second)
}

// ---------------------------------------------------------------------------
// Recording Tests
// ---------------------------------------------------------------------------

func TestRecording_StandaloneSIPLeg(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	outboundID, _ := establishCall(t, instA, instB)

	// Start recording on outbound leg (standalone, not in a room → stereo SIP tap).
	recResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/record", instA.baseURL(), outboundID), nil)
	if recResp.StatusCode != http.StatusOK {
		t.Fatalf("start recording: unexpected status %d", recResp.StatusCode)
	}
	var recStart recordingResponse
	decodeJSON(t, recResp, &recStart)
	if recStart.Status != "recording" {
		t.Fatalf("expected status 'recording', got %q", recStart.Status)
	}
	if recStart.File == "" {
		t.Fatal("expected non-empty file path")
	}
	if !strings.HasSuffix(recStart.File, ".wav") {
		t.Fatalf("expected .wav file, got %q", recStart.File)
	}

	// Verify recording.started event.
	instA.collector.waitForMatch(t, events.RecordingStarted, func(e events.Event) bool {
		return e.Data["leg_id"] == outboundID
	}, 3*time.Second)

	// Let it record briefly.
	time.Sleep(300 * time.Millisecond)

	// Stop recording.
	stopResp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s/record", instA.baseURL(), outboundID))
	if stopResp.StatusCode != http.StatusOK {
		t.Fatalf("stop recording: unexpected status %d", stopResp.StatusCode)
	}
	var recStop recordingResponse
	decodeJSON(t, stopResp, &recStop)
	if recStop.Status != "stopped" {
		t.Fatalf("expected status 'stopped', got %q", recStop.Status)
	}
	if recStop.File != recStart.File {
		t.Fatalf("file path mismatch: start=%q stop=%q", recStart.File, recStop.File)
	}

	// Verify recording.finished event.
	instA.collector.waitForMatch(t, events.RecordingFinished, func(e events.Event) bool {
		return e.Data["leg_id"] == outboundID
	}, 3*time.Second)

	// Verify WAV contains stereo 8kHz audio with real samples.
	assertWAVAudio(t, recStart.File, 2, 8000, 100)

	// Cleanup.
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
}

func TestRecording_InRoomLeg(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	outboundID, _ := establishCall(t, instA, instB)

	// Create room and add the outbound leg.
	roomResp := httpPost(t, instA.baseURL()+"/v1/rooms", map[string]interface{}{})
	if roomResp.StatusCode != http.StatusCreated {
		t.Fatalf("create room: unexpected status %d", roomResp.StatusCode)
	}
	var rm roomView
	decodeJSON(t, roomResp, &rm)

	addResp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/legs", instA.baseURL(), rm.ID), map[string]interface{}{
		"leg_id": outboundID,
	})
	if addResp.StatusCode != http.StatusOK {
		t.Fatalf("add leg to room: unexpected status %d", addResp.StatusCode)
	}
	addResp.Body.Close()
	instA.collector.waitForMatch(t, events.LegJoinedRoom, nil, 3*time.Second)

	// Start recording on the leg (now in a room → stereo mixer taps).
	recResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/record", instA.baseURL(), outboundID), nil)
	if recResp.StatusCode != http.StatusOK {
		t.Fatalf("start recording: unexpected status %d", recResp.StatusCode)
	}
	var recStart recordingResponse
	decodeJSON(t, recResp, &recStart)
	if recStart.Status != "recording" {
		t.Fatalf("expected status 'recording', got %q", recStart.Status)
	}

	instA.collector.waitForMatch(t, events.RecordingStarted, func(e events.Event) bool {
		return e.Data["leg_id"] == outboundID
	}, 3*time.Second)

	time.Sleep(300 * time.Millisecond)

	// Stop recording.
	stopResp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s/record", instA.baseURL(), outboundID))
	if stopResp.StatusCode != http.StatusOK {
		t.Fatalf("stop recording: unexpected status %d", stopResp.StatusCode)
	}
	var recStop recordingResponse
	decodeJSON(t, stopResp, &recStop)
	if recStop.Status != "stopped" {
		t.Fatalf("expected status 'stopped', got %q", recStop.Status)
	}

	instA.collector.waitForMatch(t, events.RecordingFinished, func(e events.Event) bool {
		return e.Data["leg_id"] == outboundID
	}, 3*time.Second)

	// Verify WAV contains stereo 16kHz audio (mixer rate) with real samples.
	assertWAVAudio(t, recStart.File, 2, 16000, 100)

	// Cleanup.
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
}

func TestRecording_Room(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	outboundID, _ := establishCall(t, instA, instB)

	// Create room and add the outbound leg (room recording requires at least one participant).
	roomResp := httpPost(t, instA.baseURL()+"/v1/rooms", map[string]interface{}{})
	if roomResp.StatusCode != http.StatusCreated {
		t.Fatalf("create room: unexpected status %d", roomResp.StatusCode)
	}
	var rm roomView
	decodeJSON(t, roomResp, &rm)

	addResp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/legs", instA.baseURL(), rm.ID), map[string]interface{}{
		"leg_id": outboundID,
	})
	if addResp.StatusCode != http.StatusOK {
		t.Fatalf("add leg to room: unexpected status %d", addResp.StatusCode)
	}
	addResp.Body.Close()
	instA.collector.waitForMatch(t, events.LegJoinedRoom, nil, 3*time.Second)

	// Start room recording.
	recResp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/record", instA.baseURL(), rm.ID), nil)
	if recResp.StatusCode != http.StatusOK {
		t.Fatalf("start room recording: unexpected status %d", recResp.StatusCode)
	}
	var recStart recordingResponse
	decodeJSON(t, recResp, &recStart)
	if recStart.Status != "recording" {
		t.Fatalf("expected status 'recording', got %q", recStart.Status)
	}
	if !strings.HasSuffix(recStart.File, ".wav") {
		t.Fatalf("expected .wav file, got %q", recStart.File)
	}

	instA.collector.waitForMatch(t, events.RecordingStarted, func(e events.Event) bool {
		return e.Data["room_id"] == rm.ID
	}, 3*time.Second)

	time.Sleep(300 * time.Millisecond)

	// Stop room recording.
	stopResp := httpDelete(t, fmt.Sprintf("%s/v1/rooms/%s/record", instA.baseURL(), rm.ID))
	if stopResp.StatusCode != http.StatusOK {
		t.Fatalf("stop room recording: unexpected status %d", stopResp.StatusCode)
	}
	var recStop recordingResponse
	decodeJSON(t, stopResp, &recStop)
	if recStop.Status != "stopped" {
		t.Fatalf("expected status 'stopped', got %q", recStop.Status)
	}
	if recStop.File != recStart.File {
		t.Fatalf("file path mismatch: start=%q stop=%q", recStart.File, recStop.File)
	}

	instA.collector.waitForMatch(t, events.RecordingFinished, func(e events.Event) bool {
		return e.Data["room_id"] == rm.ID
	}, 3*time.Second)

	// Verify WAV contains mono 16kHz audio (mixer rate) with real samples.
	assertWAVAudio(t, recStart.File, 1, 16000, 100)

	// Cleanup.
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
}

func TestRecording_StopsOnDisconnect(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	outboundID, _ := establishCall(t, instA, instB)

	// Start recording.
	recResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/record", instA.baseURL(), outboundID), nil)
	if recResp.StatusCode != http.StatusOK {
		t.Fatalf("start recording: unexpected status %d", recResp.StatusCode)
	}
	var recStart recordingResponse
	decodeJSON(t, recResp, &recStart)

	instA.collector.waitForMatch(t, events.RecordingStarted, func(e events.Event) bool {
		return e.Data["leg_id"] == outboundID
	}, 3*time.Second)

	time.Sleep(200 * time.Millisecond)

	// Hangup the leg — recording should auto-stop via cleanupLeg.
	delResp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("delete leg: unexpected status %d", delResp.StatusCode)
	}
	delResp.Body.Close()

	// Verify recording.finished event fires automatically.
	instA.collector.waitForMatch(t, events.RecordingFinished, func(e events.Event) bool {
		return e.Data["leg_id"] == outboundID
	}, 5*time.Second)

	// Verify WAV contains stereo 8kHz audio with real samples.
	assertWAVAudio(t, recStart.File, 2, 8000, 100)
}

func TestRecording_RoomNoParticipants(t *testing.T) {
	instA := newTestInstance(t, "instance-a")

	// Create an empty room.
	roomResp := httpPost(t, instA.baseURL()+"/v1/rooms", map[string]interface{}{})
	if roomResp.StatusCode != http.StatusCreated {
		t.Fatalf("create room: unexpected status %d", roomResp.StatusCode)
	}
	var rm roomView
	decodeJSON(t, roomResp, &rm)

	// Attempt to record — should fail with 409 (room has no participants).
	recResp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/record", instA.baseURL(), rm.ID), nil)
	if recResp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict, got %d", recResp.StatusCode)
	}
	recResp.Body.Close()
}

// ---------------------------------------------------------------------------
// Mute Tests
// ---------------------------------------------------------------------------

func TestMute_LegInRoom(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	outboundID, _ := establishCall(t, instA, instB)

	// Create room and add leg.
	roomResp := httpPost(t, instA.baseURL()+"/v1/rooms", map[string]interface{}{})
	var rm roomView
	decodeJSON(t, roomResp, &rm)

	addResp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/legs", instA.baseURL(), rm.ID), map[string]interface{}{
		"leg_id": outboundID,
	})
	addResp.Body.Close()
	instA.collector.waitForMatch(t, events.LegJoinedRoom, nil, 3*time.Second)

	// Mute the leg.
	muteResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/mute", instA.baseURL(), outboundID), nil)
	if muteResp.StatusCode != http.StatusOK {
		t.Fatalf("mute: unexpected status %d", muteResp.StatusCode)
	}
	var muteResult map[string]string
	decodeJSON(t, muteResp, &muteResult)
	if muteResult["status"] != "muted" {
		t.Fatalf("expected status 'muted', got %q", muteResult["status"])
	}

	// Verify leg.muted event.
	instA.collector.waitForMatch(t, events.LegMuted, func(e events.Event) bool {
		return e.Data["leg_id"] == outboundID
	}, 3*time.Second)

	// Verify GET shows muted.
	getResp := httpGet(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
	var gotLeg legView
	decodeJSON(t, getResp, &gotLeg)
	if !gotLeg.Muted {
		t.Fatal("expected leg to be muted")
	}

	// Unmute the leg.
	unmuteResp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s/mute", instA.baseURL(), outboundID))
	if unmuteResp.StatusCode != http.StatusOK {
		t.Fatalf("unmute: unexpected status %d", unmuteResp.StatusCode)
	}
	var unmuteResult map[string]string
	decodeJSON(t, unmuteResp, &unmuteResult)
	if unmuteResult["status"] != "unmuted" {
		t.Fatalf("expected status 'unmuted', got %q", unmuteResult["status"])
	}

	// Verify leg.unmuted event.
	instA.collector.waitForMatch(t, events.LegUnmuted, func(e events.Event) bool {
		return e.Data["leg_id"] == outboundID
	}, 3*time.Second)

	// Verify GET shows unmuted.
	getResp2 := httpGet(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
	var gotLeg2 legView
	decodeJSON(t, getResp2, &gotLeg2)
	if gotLeg2.Muted {
		t.Fatal("expected leg to be unmuted")
	}

	// Cleanup.
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
}

func TestMute_SpeakingEventsSuppressed(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	outboundID, _ := establishCall(t, instA, instB)

	// Create room and add leg.
	roomResp := httpPost(t, instA.baseURL()+"/v1/rooms", map[string]interface{}{})
	var rm roomView
	decodeJSON(t, roomResp, &rm)

	addResp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/legs", instA.baseURL(), rm.ID), map[string]interface{}{
		"leg_id": outboundID,
	})
	addResp.Body.Close()
	instA.collector.waitForMatch(t, events.LegJoinedRoom, nil, 3*time.Second)

	// Mute the leg.
	muteResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/mute", instA.baseURL(), outboundID), nil)
	muteResp.Body.Close()
	instA.collector.waitForMatch(t, events.LegMuted, nil, 3*time.Second)

	// Wait a bit — no speaking events should fire for the muted leg.
	time.Sleep(500 * time.Millisecond)

	if instA.collector.hasEvent(events.SpeakingStarted, func(e events.Event) bool {
		return e.Data["leg_id"] == outboundID
	}) {
		t.Fatal("speaking.started should not fire for muted leg")
	}

	// Cleanup.
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
}

func TestMute_BeforeRoomJoin(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	outboundID, _ := establishCall(t, instA, instB)

	// Mute before joining room.
	muteResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/mute", instA.baseURL(), outboundID), nil)
	if muteResp.StatusCode != http.StatusOK {
		t.Fatalf("mute: unexpected status %d", muteResp.StatusCode)
	}
	muteResp.Body.Close()
	instA.collector.waitForMatch(t, events.LegMuted, nil, 3*time.Second)

	// Now create room and add the already-muted leg.
	roomResp := httpPost(t, instA.baseURL()+"/v1/rooms", map[string]interface{}{})
	var rm roomView
	decodeJSON(t, roomResp, &rm)

	addResp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/legs", instA.baseURL(), rm.ID), map[string]interface{}{
		"leg_id": outboundID,
	})
	addResp.Body.Close()
	instA.collector.waitForMatch(t, events.LegJoinedRoom, nil, 3*time.Second)

	// Verify GET still shows muted.
	getResp := httpGet(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
	var gotLeg legView
	decodeJSON(t, getResp, &gotLeg)
	if !gotLeg.Muted {
		t.Fatal("expected leg to still be muted after room join")
	}

	// Wait a bit — no speaking events should fire.
	time.Sleep(500 * time.Millisecond)

	if instA.collector.hasEvent(events.SpeakingStarted, func(e events.Event) bool {
		return e.Data["leg_id"] == outboundID
	}) {
		t.Fatal("speaking.started should not fire for pre-muted leg")
	}

	// Cleanup.
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
}

// ---------------------------------------------------------------------------
// Early Media Tests
// ---------------------------------------------------------------------------

func TestOutboundEarlyMedia_183WithSDP(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")

	// A dials B.
	createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type":   "sip",
		"uri":    fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"codecs": []string{"PCMU"},
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: unexpected status %d", createResp.StatusCode)
	}
	var outboundLeg legView
	decodeJSON(t, createResp, &outboundLeg)
	t.Logf("outbound leg: %s", outboundLeg.ID)

	// Wait for inbound leg on B (ringing).
	inboundLeg := waitForInboundLeg(t, instB.baseURL(), 5*time.Second)
	t.Logf("inbound leg: %s", inboundLeg.ID)

	// B enables early media → sends 183 Session Progress with SDP.
	emResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/early-media", instB.baseURL(), inboundLeg.ID), nil)
	if emResp.StatusCode != http.StatusOK {
		t.Fatalf("early-media: unexpected status %d", emResp.StatusCode)
	}
	emResp.Body.Close()

	// A's outbound leg should transition to early_media.
	waitForLegState(t, instA.baseURL(), outboundLeg.ID, "early_media", 5*time.Second)

	// Verify leg.early_media event on A.
	instA.collector.waitForMatch(t, events.LegEarlyMedia, func(e events.Event) bool {
		return e.Data["leg_id"] == outboundLeg.ID
	}, 3*time.Second)

	// Create room on A and add the early_media leg — should succeed.
	roomResp := httpPost(t, instA.baseURL()+"/v1/rooms", map[string]interface{}{})
	if roomResp.StatusCode != http.StatusCreated {
		t.Fatalf("create room: unexpected status %d", roomResp.StatusCode)
	}
	var rm roomView
	decodeJSON(t, roomResp, &rm)
	t.Logf("room: %s", rm.ID)

	addResp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/legs", instA.baseURL(), rm.ID), map[string]interface{}{
		"leg_id": outboundLeg.ID,
	})
	if addResp.StatusCode != http.StatusOK {
		t.Fatalf("add early_media leg to room: unexpected status %d", addResp.StatusCode)
	}
	addResp.Body.Close()

	instA.collector.waitForMatch(t, events.LegJoinedRoom, func(e events.Event) bool {
		return e.Data["leg_id"] == outboundLeg.ID && e.Data["room_id"] == rm.ID
	}, 3*time.Second)

	// B answers — A's outbound leg should transition from early_media to connected.
	answerResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/answer", instB.baseURL(), inboundLeg.ID), nil)
	if answerResp.StatusCode != http.StatusOK {
		t.Fatalf("answer: unexpected status %d", answerResp.StatusCode)
	}
	answerResp.Body.Close()

	waitForLegState(t, instA.baseURL(), outboundLeg.ID, "connected", 5*time.Second)
	waitForLegState(t, instB.baseURL(), inboundLeg.ID, "connected", 5*time.Second)

	// Verify leg.connected event on A.
	instA.collector.waitForMatch(t, events.LegConnected, func(e events.Event) bool {
		return e.Data["leg_id"] == outboundLeg.ID
	}, 3*time.Second)

	// Verify room still has the participant.
	getRoomResp := httpGet(t, fmt.Sprintf("%s/v1/rooms/%s", instA.baseURL(), rm.ID))
	var gotRoom roomView
	decodeJSON(t, getRoomResp, &gotRoom)
	if len(gotRoom.Participants) != 1 {
		t.Fatalf("expected 1 participant, got %d", len(gotRoom.Participants))
	}
	if gotRoom.Participants[0].State != "connected" {
		t.Fatalf("expected participant state connected, got %s", gotRoom.Participants[0].State)
	}

	// Cleanup.
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundLeg.ID))
}

func TestOutboundEarlyMedia_AnswerWithoutEarlyMedia(t *testing.T) {
	// Verify the normal path (no 183) still works correctly — leg goes
	// directly from ringing to connected without early_media.
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")

	outboundID, inboundID := establishCall(t, instA, instB)

	// Ensure no early_media event was emitted on A.
	if instA.collector.hasEvent(events.LegEarlyMedia, func(e events.Event) bool {
		return e.Data["leg_id"] == outboundID
	}) {
		t.Fatal("leg.early_media should not fire when remote answers directly")
	}

	// Both legs should be connected.
	waitForLegState(t, instA.baseURL(), outboundID, "connected", 3*time.Second)
	waitForLegState(t, instB.baseURL(), inboundID, "connected", 3*time.Second)

	// Cleanup.
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
}

func TestOutboundEarlyMedia_HangupDuringEarlyMedia(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")

	// A dials B.
	createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type":   "sip",
		"uri":    fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"codecs": []string{"PCMU"},
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: unexpected status %d", createResp.StatusCode)
	}
	var outboundLeg legView
	decodeJSON(t, createResp, &outboundLeg)

	// Wait for inbound on B, enable early media.
	inboundLeg := waitForInboundLeg(t, instB.baseURL(), 5*time.Second)

	emResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/early-media", instB.baseURL(), inboundLeg.ID), nil)
	if emResp.StatusCode != http.StatusOK {
		t.Fatalf("early-media: unexpected status %d", emResp.StatusCode)
	}
	emResp.Body.Close()

	// Wait for A's leg to reach early_media.
	waitForLegState(t, instA.baseURL(), outboundLeg.ID, "early_media", 5*time.Second)

	// A hangs up during early media (before answer).
	delResp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundLeg.ID))
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("delete leg: unexpected status %d", delResp.StatusCode)
	}
	delResp.Body.Close()

	// Verify disconnected event on A.
	instA.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data["leg_id"] == outboundLeg.ID
	}, 5*time.Second)

	// Verify B also sees disconnect.
	instB.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data["leg_id"] == inboundLeg.ID
	}, 5*time.Second)
}

func TestRecording_StopWithNoRecording(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	outboundID, _ := establishCall(t, instA, instB)

	// Stop recording when none is in progress — should return 404.
	stopResp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s/record", instA.baseURL(), outboundID))
	if stopResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", stopResp.StatusCode)
	}
	stopResp.Body.Close()

	// Cleanup.
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
}

func TestRecording_StorageFileExplicit(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	outboundID, _ := establishCall(t, instA, instB)

	// Start recording with explicit storage=file.
	recResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/record", instA.baseURL(), outboundID),
		map[string]string{"storage": "file"})
	if recResp.StatusCode != http.StatusOK {
		t.Fatalf("start recording: unexpected status %d", recResp.StatusCode)
	}
	var recStart recordingResponse
	decodeJSON(t, recResp, &recStart)
	if recStart.Status != "recording" {
		t.Fatalf("expected status 'recording', got %q", recStart.Status)
	}
	if !strings.HasSuffix(recStart.File, ".wav") {
		t.Fatalf("expected .wav file, got %q", recStart.File)
	}

	time.Sleep(300 * time.Millisecond)

	// Stop recording.
	stopResp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s/record", instA.baseURL(), outboundID))
	if stopResp.StatusCode != http.StatusOK {
		t.Fatalf("stop recording: unexpected status %d", stopResp.StatusCode)
	}
	var recStop recordingResponse
	decodeJSON(t, stopResp, &recStop)
	if recStop.Status != "stopped" {
		t.Fatalf("expected status 'stopped', got %q", recStop.Status)
	}
	if recStop.File != recStart.File {
		t.Fatalf("file path mismatch: start=%q stop=%q", recStart.File, recStop.File)
	}

	// Verify WAV is valid.
	assertWAVAudio(t, recStart.File, 2, 8000, 100)

	// Cleanup.
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
}

func TestRecording_StorageS3NotConfigured(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	outboundID, _ := establishCall(t, instA, instB)

	// Request storage=s3 when S3 is not configured — should return 400.
	recResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/record", instA.baseURL(), outboundID),
		map[string]string{"storage": "s3"})
	if recResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", recResp.StatusCode)
	}
	recResp.Body.Close()

	// Cleanup.
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
}
