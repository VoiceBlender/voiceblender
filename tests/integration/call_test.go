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

	"github.com/VoiceBlender/voiceblender/internal/api"
	"github.com/VoiceBlender/voiceblender/internal/codec"
	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/VoiceBlender/voiceblender/internal/room"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
	goaudio "github.com/go-audio/audio"
	"github.com/go-audio/wav"
)

// ---------------------------------------------------------------------------
// testInstance — encapsulates one full VoiceBlender stack
// ---------------------------------------------------------------------------

type testInstance struct {
	name      string
	cfg       config.Config
	bus       *events.Bus
	webhooks  *events.WebhookRegistry
	legMgr    *leg.Manager
	roomMgr   *room.Manager
	engine    *sipmod.Engine
	apiSrv    *api.Server
	httpSrv   *http.Server
	httpAddr  string // "127.0.0.1:<port>"
	sipPort   int
	collector *eventCollector
	cancel    context.CancelFunc
}

func newTestInstance(t *testing.T, name string) *testInstance {
	return newTestInstanceFull(t, name, nil, nil)
}

// newTestInstanceWithOpts is like newTestInstance but lets the test mutate
// the config (e.g. enable SIP_REFER_AUTO_DIAL) before the server is wired.
func newTestInstanceWithOpts(t *testing.T, name string, mutate func(*config.Config)) *testInstance {
	return newTestInstanceFull(t, name, mutate, nil)
}

// newTestInstanceWithCodecs is like newTestInstance but overrides the SIP
// engine's advertised codec list. Pass nil for the default (PCMU).
func newTestInstanceWithCodecs(t *testing.T, name string, codecs []codec.CodecType) *testInstance {
	return newTestInstanceFull(t, name, nil, codecs)
}

func newTestInstanceFull(t *testing.T, name string, mutate func(*config.Config), codecs []codec.CodecType) *testInstance {
	t.Helper()

	if len(codecs) == 0 {
		codecs = []codec.CodecType{codec.CodecPCMU}
	}

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
		SIPBindIP:         "127.0.0.1",
		SIPListenIP:       "127.0.0.1",
		SIPPort:           fmt.Sprintf("%d", sipPort),
		SIPHost:           name,
		HTTPAddr:          "127.0.0.1:0",
		RecordingDir:      recDir,
		DefaultSampleRate: 16000,
	}
	if mutate != nil {
		mutate(&cfg)
	}

	bus := events.NewBus("test")
	webhooks := events.NewWebhookRegistry(bus, log, "", "")
	legMgr := leg.NewManager()
	roomMgr := room.NewManager(legMgr, bus, log)

	engine, err := sipmod.NewEngine(sipmod.EngineConfig{
		BindIP:     "127.0.0.1",
		ListenIP:   "127.0.0.1",
		ExternalIP: cfg.SIPExternalIP,
		BindPort:   sipPort,
		SIPHost:    name,
		Codecs:     codecs,
		Log:        log,
	})
	if err != nil {
		t.Fatalf("[%s] new engine: %v", name, err)
	}

	apiSrv := api.NewServer(legMgr, roomMgr, engine, bus, webhooks, nil, nil, nil, nil, cfg, log)
	engine.OnInvite(apiSrv.HandleInboundCall)
	engine.OnReInvite(apiSrv.HandleReInvite)
	engine.OnRefer(apiSrv.HandleIncomingRefer)
	engine.OnNotify(apiSrv.HandleReferNotify)

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
	_ = bus.Subscribe(ec.handle)

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

func (ec *eventCollector) matchAll(typ events.EventType, pred func(events.Event) bool) []events.Event {
	ec.mu.Lock()
	defer ec.mu.Unlock()
	var result []events.Event
	for _, e := range ec.events {
		if e.Type == typ && (pred == nil || pred(e)) {
			result = append(result, e)
		}
	}
	return result
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
	ID         string            `json:"id"`
	Type       string            `json:"type"`
	State      string            `json:"state"`
	RoomID     string            `json:"room_id,omitempty"`
	Muted      bool              `json:"muted"`
	Deaf       bool              `json:"deaf"`
	Held       bool              `json:"held"`
	SIPHeaders map[string]string `json:"sip_headers,omitempty"`
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
		return e.Data.GetLegID() == outboundLeg.ID
	}, 5*time.Second)
	instB.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == inboundLeg.ID
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
		return e.Data.GetLegID() == outboundID
	}, 5*time.Second)

	dA := eA.Data.(*events.LegDisconnectedData)
	dTotal := dA.CDR.DurationTotal
	if dTotal < 0.1 {
		t.Fatalf("expected duration_total >= 0.1s, got %f", dTotal)
	}

	dAnswered := dA.CDR.DurationAnswered
	if dAnswered < 0.1 {
		t.Fatalf("expected duration_answered >= 0.1s, got %f", dAnswered)
	}
	if dAnswered > dTotal {
		t.Fatalf("duration_answered (%f) > duration_total (%f)", dAnswered, dTotal)
	}

	t.Logf("outbound: duration_total=%.3fs duration_answered=%.3fs", dTotal, dAnswered)

	// Verify inbound disconnect event on B also has duration fields.
	eB := instB.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == inboundID
	}, 5*time.Second)

	dB := eB.Data.(*events.LegDisconnectedData)
	dTotalB := dB.CDR.DurationTotal
	if dTotalB < 0.1 {
		t.Fatalf("B: expected duration_total >= 0.1s, got %f", dTotalB)
	}

	dAnsweredB := dB.CDR.DurationAnswered
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
		return e.Data.GetLegID() == outboundLeg.ID
	}, 5*time.Second)

	// duration_total should be > 0 (at least the ring timeout).
	d := e.Data.(*events.LegDisconnectedData)
	dTotal := d.CDR.DurationTotal
	if dTotal < 0.5 {
		t.Fatalf("expected duration_total >= 0.5s, got %f", dTotal)
	}

	// duration_answered should be 0 (never answered).
	dAnswered := d.CDR.DurationAnswered
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
		return e.Data.GetLegID() == inboundLeg.ID && e.Data.(*events.LegDisconnectedData).CDR.Reason == "caller_cancel"
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
		return e.Data.GetLegID() == outboundLeg.ID && e.Data.GetRoomID() == rm.ID
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
		return e.Data.GetLegID() == outboundLeg.ID && e.Data.GetRoomID() == rm.ID
	}, 3*time.Second)

	// Cleanup: hangup.
	delResp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundLeg.ID))
	delResp.Body.Close()
}

func TestRoom_MoveLegViaPost(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	outboundID, _ := establishCall(t, instA, instB)

	// Create room-1 and add the outbound leg.
	roomResp1 := httpPost(t, instA.baseURL()+"/v1/rooms", map[string]interface{}{"id": "room-move-1"})
	if roomResp1.StatusCode != http.StatusCreated {
		t.Fatalf("create room-1: unexpected status %d", roomResp1.StatusCode)
	}
	roomResp1.Body.Close()

	addResp := httpPost(t, fmt.Sprintf("%s/v1/rooms/room-move-1/legs", instA.baseURL()), map[string]interface{}{
		"leg_id": outboundID,
	})
	if addResp.StatusCode != http.StatusOK {
		t.Fatalf("add leg to room-1: unexpected status %d", addResp.StatusCode)
	}
	var addResult map[string]string
	decodeJSON(t, addResp, &addResult)
	if addResult["status"] != "added" {
		t.Fatalf("expected status 'added', got %q", addResult["status"])
	}

	instA.collector.waitForMatch(t, events.LegJoinedRoom, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundID && e.Data.GetRoomID() == "room-move-1"
	}, 3*time.Second)

	// Create room-2.
	roomResp2 := httpPost(t, instA.baseURL()+"/v1/rooms", map[string]interface{}{"id": "room-move-2"})
	if roomResp2.StatusCode != http.StatusCreated {
		t.Fatalf("create room-2: unexpected status %d", roomResp2.StatusCode)
	}
	roomResp2.Body.Close()

	// Move leg from room-1 to room-2 via POST /v1/rooms/room-move-2/legs.
	moveResp := httpPost(t, fmt.Sprintf("%s/v1/rooms/room-move-2/legs", instA.baseURL()), map[string]interface{}{
		"leg_id": outboundID,
	})
	if moveResp.StatusCode != http.StatusOK {
		t.Fatalf("move leg to room-2: unexpected status %d", moveResp.StatusCode)
	}
	var moveResult map[string]string
	decodeJSON(t, moveResp, &moveResult)
	if moveResult["status"] != "moved" {
		t.Fatalf("expected status 'moved', got %q", moveResult["status"])
	}
	if moveResult["from"] != "room-move-1" {
		t.Fatalf("expected from 'room-move-1', got %q", moveResult["from"])
	}
	if moveResult["to"] != "room-move-2" {
		t.Fatalf("expected to 'room-move-2', got %q", moveResult["to"])
	}

	// Verify leg.left_room event from room-1.
	instA.collector.waitForMatch(t, events.LegLeftRoom, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundID && e.Data.GetRoomID() == "room-move-1"
	}, 3*time.Second)

	// Verify leg.joined_room event for room-2.
	instA.collector.waitForMatch(t, events.LegJoinedRoom, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundID && e.Data.GetRoomID() == "room-move-2"
	}, 3*time.Second)

	// Verify room-2 now has the participant.
	getRoomResp := httpGet(t, fmt.Sprintf("%s/v1/rooms/room-move-2", instA.baseURL()))
	var gotRoom roomView
	decodeJSON(t, getRoomResp, &gotRoom)
	if len(gotRoom.Participants) != 1 {
		t.Fatalf("expected 1 participant in room-2, got %d", len(gotRoom.Participants))
	}
	if gotRoom.Participants[0].ID != outboundID {
		t.Fatalf("expected participant %s, got %s", outboundID, gotRoom.Participants[0].ID)
	}

	// Verify room-1 is now empty.
	getRoom1Resp := httpGet(t, fmt.Sprintf("%s/v1/rooms/room-move-1", instA.baseURL()))
	var gotRoom1 roomView
	decodeJSON(t, getRoom1Resp, &gotRoom1)
	if len(gotRoom1.Participants) != 0 {
		t.Fatalf("expected 0 participants in room-1, got %d", len(gotRoom1.Participants))
	}

	// Verify adding leg to the same room returns 400.
	dupResp := httpPost(t, fmt.Sprintf("%s/v1/rooms/room-move-2/legs", instA.baseURL()), map[string]interface{}{
		"leg_id": outboundID,
	})
	if dupResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for duplicate add, got %d", dupResp.StatusCode)
	}
	dupResp.Body.Close()

	// Cleanup.
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
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
		return e.Data.GetLegID() == outboundLeg.ID
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
		return e.Data.GetLegID() == outboundID
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
		return e.Data.GetLegID() == outboundID
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
		return e.Data.GetLegID() == outboundID
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
		return e.Data.GetLegID() == outboundID
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
		return e.Data.GetRoomID() == rm.ID
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
		return e.Data.GetRoomID() == rm.ID
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
		return e.Data.GetLegID() == outboundID
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
		return e.Data.GetLegID() == outboundID
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
		return e.Data.GetLegID() == outboundID
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
		return e.Data.GetLegID() == outboundID
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

// TestAddLegToRoom_JoinMutedAndDeaf verifies that a leg can be added to a
// room pre-muted and pre-deafened in a single atomic request, with no
// intermediate window where un-muted audio enters the mix.
func TestAddLegToRoom_JoinMutedAndDeaf(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	outboundID, _ := establishCall(t, instA, instB)

	roomResp := httpPost(t, instA.baseURL()+"/v1/rooms", map[string]interface{}{})
	var rm roomView
	decodeJSON(t, roomResp, &rm)

	addResp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/legs", instA.baseURL(), rm.ID), map[string]interface{}{
		"leg_id": outboundID,
		"mute":   true,
		"deaf":   true,
	})
	if addResp.StatusCode != http.StatusOK {
		t.Fatalf("add leg: unexpected status %d", addResp.StatusCode)
	}
	addResp.Body.Close()
	instA.collector.waitForMatch(t, events.LegJoinedRoom, nil, 3*time.Second)

	getResp := httpGet(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
	var gotLeg legView
	decodeJSON(t, getResp, &gotLeg)
	if !gotLeg.Muted {
		t.Error("expected leg to be muted after join")
	}
	if !gotLeg.Deaf {
		t.Error("expected leg to be deaf after join")
	}

	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
}

func TestMute_SpeakingEventsSuppressed(t *testing.T) {
	instA := newTestInstanceWithOpts(t, "instance-a", func(c *config.Config) { c.SpeechDetectionEnabled = true })
	instB := newTestInstanceWithOpts(t, "instance-b", func(c *config.Config) { c.SpeechDetectionEnabled = true })
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
		return e.Data.GetLegID() == outboundID
	}) {
		t.Fatal("speaking.started should not fire for muted leg")
	}

	// Cleanup.
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
}

func TestMute_BeforeRoomJoin(t *testing.T) {
	instA := newTestInstanceWithOpts(t, "instance-a", func(c *config.Config) { c.SpeechDetectionEnabled = true })
	instB := newTestInstanceWithOpts(t, "instance-b", func(c *config.Config) { c.SpeechDetectionEnabled = true })
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
		return e.Data.GetLegID() == outboundID
	}) {
		t.Fatal("speaking.started should not fire for pre-muted leg")
	}

	// Cleanup.
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
}

// ---------------------------------------------------------------------------
// Hold Tests
// ---------------------------------------------------------------------------

// TestHold_LocalHoldUnhold verifies that POST /v1/legs/{id}/hold puts the
// call on hold (state=held, held=true, leg.hold event) and DELETE
// /v1/legs/{id}/hold resumes it (state=connected, held=false, leg.unhold event).
func TestHold_LocalHoldUnhold(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	outboundID, _ := establishCall(t, instA, instB)

	// Hold the leg.
	holdResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/hold", instA.baseURL(), outboundID), nil)
	if holdResp.StatusCode != http.StatusOK {
		t.Fatalf("hold: unexpected status %d", holdResp.StatusCode)
	}
	var holdResult map[string]string
	decodeJSON(t, holdResp, &holdResult)
	if holdResult["status"] != "held" {
		t.Fatalf("expected status 'held', got %q", holdResult["status"])
	}

	// Verify leg.hold event.
	instA.collector.waitForMatch(t, events.LegHold, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundID
	}, 3*time.Second)

	// Verify GET shows held=true, state=held.
	getResp := httpGet(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
	var gotLeg legView
	decodeJSON(t, getResp, &gotLeg)
	if !gotLeg.Held {
		t.Fatal("expected leg to be held")
	}
	if gotLeg.State != "held" {
		t.Fatalf("expected state 'held', got %q", gotLeg.State)
	}

	// Unhold the leg.
	unholdResp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s/hold", instA.baseURL(), outboundID))
	if unholdResp.StatusCode != http.StatusOK {
		t.Fatalf("unhold: unexpected status %d", unholdResp.StatusCode)
	}
	var unholdResult map[string]string
	decodeJSON(t, unholdResp, &unholdResult)
	if unholdResult["status"] != "resumed" {
		t.Fatalf("expected status 'resumed', got %q", unholdResult["status"])
	}

	// Verify leg.unhold event.
	instA.collector.waitForMatch(t, events.LegUnhold, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundID
	}, 3*time.Second)

	// Verify GET shows held=false, state=connected.
	getResp2 := httpGet(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
	var gotLeg2 legView
	decodeJSON(t, getResp2, &gotLeg2)
	if gotLeg2.Held {
		t.Fatal("expected leg to not be held")
	}
	if gotLeg2.State != "connected" {
		t.Fatalf("expected state 'connected', got %q", gotLeg2.State)
	}

	// Cleanup.
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
}

// TestHold_DoubleHoldNoop verifies that holding an already-held leg is a no-op.
func TestHold_DoubleHoldNoop(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	outboundID, _ := establishCall(t, instA, instB)

	// Hold the leg twice.
	holdResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/hold", instA.baseURL(), outboundID), nil)
	if holdResp.StatusCode != http.StatusOK {
		t.Fatalf("hold: unexpected status %d", holdResp.StatusCode)
	}
	holdResp.Body.Close()
	instA.collector.waitForMatch(t, events.LegHold, nil, 3*time.Second)

	// Second hold should also succeed (no-op).
	holdResp2 := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/hold", instA.baseURL(), outboundID), nil)
	if holdResp2.StatusCode != http.StatusOK {
		t.Fatalf("double hold: unexpected status %d", holdResp2.StatusCode)
	}
	holdResp2.Body.Close()

	// Cleanup.
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
}

// TestHold_UnholdWhenNotHeld verifies that unholding a non-held leg is a no-op.
func TestHold_UnholdWhenNotHeld(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	outboundID, _ := establishCall(t, instA, instB)

	// Unhold a non-held leg should succeed (no-op).
	unholdResp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s/hold", instA.baseURL(), outboundID))
	if unholdResp.StatusCode != http.StatusOK {
		t.Fatalf("unhold not-held: unexpected status %d", unholdResp.StatusCode)
	}
	unholdResp.Body.Close()

	// Cleanup.
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
}

// TestHold_WebRTCNotSupported verifies that holding a WebRTC leg returns an error.
func TestHold_WebRTCNotSupported(t *testing.T) {
	inst := newTestInstance(t, "instance-a")

	// Try to hold a non-existent leg (will get 404).
	holdResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/hold", inst.baseURL(), "nonexistent"), nil)
	if holdResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", holdResp.StatusCode)
	}
	holdResp.Body.Close()
}

// TestHold_NotConnected verifies that holding a ringing leg returns an error.
func TestHold_NotConnected(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")

	// Create outbound leg (will be in ringing state).
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

	// Wait for it to be in ringing state on instB.
	waitForInboundLeg(t, instB.baseURL(), 5*time.Second)

	// Try to hold a ringing leg — should fail.
	holdResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/hold", instA.baseURL(), outboundLeg.ID), nil)
	if holdResp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 for hold on ringing leg, got %d", holdResp.StatusCode)
	}
	holdResp.Body.Close()

	// Cleanup.
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundLeg.ID))
}

// TestHold_HangupCleansUpHoldTimer verifies that hanging up a held leg doesn't
// cause a panic or leak from the hold timer.
func TestHold_HangupCleansUpHoldTimer(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	outboundID, _ := establishCall(t, instA, instB)

	// Hold the leg.
	holdResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/hold", instA.baseURL(), outboundID), nil)
	if holdResp.StatusCode != http.StatusOK {
		t.Fatalf("hold: unexpected status %d", holdResp.StatusCode)
	}
	holdResp.Body.Close()
	instA.collector.waitForMatch(t, events.LegHold, nil, 3*time.Second)

	// Hangup while held.
	delResp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("hangup: unexpected status %d", delResp.StatusCode)
	}
	delResp.Body.Close()

	// Verify disconnect event fires.
	instA.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundID
	}, 3*time.Second)
}

// TestHold_RemoteHoldViaReInvite verifies that when the remote side puts
// the call on hold (via re-INVITE with sendonly), the local side detects it
// and emits leg.hold / leg.unhold events.
func TestHold_RemoteHoldViaReInvite(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	outboundID, inboundID := establishCall(t, instA, instB)

	// B holds its inbound leg (simulates remote hold from A's perspective).
	holdResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/hold", instB.baseURL(), inboundID), nil)
	if holdResp.StatusCode != http.StatusOK {
		t.Fatalf("remote hold: unexpected status %d", holdResp.StatusCode)
	}
	holdResp.Body.Close()

	// B should see leg.hold event for its inbound leg.
	instB.collector.waitForMatch(t, events.LegHold, func(e events.Event) bool {
		return e.Data.GetLegID() == inboundID
	}, 3*time.Second)

	// A should detect the hold via re-INVITE and see leg.hold for its outbound leg.
	instA.collector.waitForMatch(t, events.LegHold, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundID
	}, 5*time.Second)

	// Verify A's leg shows held=true.
	getResp := httpGet(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
	var gotLeg legView
	decodeJSON(t, getResp, &gotLeg)
	if !gotLeg.Held {
		t.Fatal("expected outbound leg on A to be held (remote hold)")
	}

	// B resumes the call.
	unholdResp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s/hold", instB.baseURL(), inboundID))
	if unholdResp.StatusCode != http.StatusOK {
		t.Fatalf("remote unhold: unexpected status %d", unholdResp.StatusCode)
	}
	unholdResp.Body.Close()

	// B should see leg.unhold event.
	instB.collector.waitForMatch(t, events.LegUnhold, func(e events.Event) bool {
		return e.Data.GetLegID() == inboundID
	}, 3*time.Second)

	// A should detect the unhold.
	instA.collector.waitForMatch(t, events.LegUnhold, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundID
	}, 5*time.Second)

	// Verify A's leg is no longer held.
	getResp2 := httpGet(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
	var gotLeg2 legView
	decodeJSON(t, getResp2, &gotLeg2)
	if gotLeg2.Held {
		t.Fatal("expected outbound leg on A to not be held after unhold")
	}

	// Cleanup.
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
}

// TestHold_LegInRoom verifies hold/unhold works for a leg that is in a room.
func TestHold_LegInRoom(t *testing.T) {
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

	// Hold the leg.
	holdResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/hold", instA.baseURL(), outboundID), nil)
	if holdResp.StatusCode != http.StatusOK {
		t.Fatalf("hold: unexpected status %d", holdResp.StatusCode)
	}
	holdResp.Body.Close()

	instA.collector.waitForMatch(t, events.LegHold, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundID
	}, 3*time.Second)

	// Verify GET shows held.
	getResp := httpGet(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
	var gotLeg legView
	decodeJSON(t, getResp, &gotLeg)
	if !gotLeg.Held {
		t.Fatal("expected leg to be held")
	}

	// Unhold.
	unholdResp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s/hold", instA.baseURL(), outboundID))
	if unholdResp.StatusCode != http.StatusOK {
		t.Fatalf("unhold: unexpected status %d", unholdResp.StatusCode)
	}
	unholdResp.Body.Close()

	instA.collector.waitForMatch(t, events.LegUnhold, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundID
	}, 3*time.Second)

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
		return e.Data.GetLegID() == outboundLeg.ID
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
		return e.Data.GetLegID() == outboundLeg.ID && e.Data.GetRoomID() == rm.ID
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
		return e.Data.GetLegID() == outboundLeg.ID
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
		return e.Data.GetLegID() == outboundID
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
		return e.Data.GetLegID() == outboundLeg.ID
	}, 5*time.Second)

	// Verify B also sees disconnect.
	instB.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == inboundLeg.ID
	}, 5*time.Second)
}

func TestCreateLeg_RoomID_AutoJoinOnConnect(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")

	// Create room first.
	roomResp := httpPost(t, instA.baseURL()+"/v1/rooms", map[string]interface{}{})
	if roomResp.StatusCode != http.StatusCreated {
		t.Fatalf("create room: unexpected status %d", roomResp.StatusCode)
	}
	var rm roomView
	decodeJSON(t, roomResp, &rm)
	t.Logf("room: %s", rm.ID)

	// A dials B with room_id — leg should auto-join room once connected.
	createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type":    "sip",
		"uri":     fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"codecs":  []string{"PCMU"},
		"room_id": rm.ID,
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: unexpected status %d", createResp.StatusCode)
	}
	var outboundLeg legView
	decodeJSON(t, createResp, &outboundLeg)

	// B answers directly (no early media).
	inboundLeg := waitForInboundLeg(t, instB.baseURL(), 5*time.Second)
	answerResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/answer", instB.baseURL(), inboundLeg.ID), nil)
	if answerResp.StatusCode != http.StatusOK {
		t.Fatalf("answer: unexpected status %d", answerResp.StatusCode)
	}
	answerResp.Body.Close()

	waitForLegState(t, instA.baseURL(), outboundLeg.ID, "connected", 5*time.Second)

	// Verify leg.joined_room event.
	instA.collector.waitForMatch(t, events.LegJoinedRoom, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundLeg.ID && e.Data.GetRoomID() == rm.ID
	}, 3*time.Second)

	// Verify room shows the participant.
	getRoomResp := httpGet(t, fmt.Sprintf("%s/v1/rooms/%s", instA.baseURL(), rm.ID))
	var gotRoom roomView
	decodeJSON(t, getRoomResp, &gotRoom)
	if len(gotRoom.Participants) != 1 {
		t.Fatalf("expected 1 participant, got %d", len(gotRoom.Participants))
	}
	if gotRoom.Participants[0].ID != outboundLeg.ID {
		t.Fatalf("expected participant %s, got %s", outboundLeg.ID, gotRoom.Participants[0].ID)
	}

	// Cleanup.
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundLeg.ID))
}

func TestCreateLeg_RoomID_AutoJoinOnEarlyMedia(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")

	// Create room first.
	roomResp := httpPost(t, instA.baseURL()+"/v1/rooms", map[string]interface{}{})
	if roomResp.StatusCode != http.StatusCreated {
		t.Fatalf("create room: unexpected status %d", roomResp.StatusCode)
	}
	var rm roomView
	decodeJSON(t, roomResp, &rm)

	// A dials B with room_id.
	createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type":    "sip",
		"uri":     fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"codecs":  []string{"PCMU"},
		"room_id": rm.ID,
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: unexpected status %d", createResp.StatusCode)
	}
	var outboundLeg legView
	decodeJSON(t, createResp, &outboundLeg)

	// B enables early media → leg should auto-join room during early_media.
	inboundLeg := waitForInboundLeg(t, instB.baseURL(), 5*time.Second)
	emResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/early-media", instB.baseURL(), inboundLeg.ID), nil)
	if emResp.StatusCode != http.StatusOK {
		t.Fatalf("early-media: unexpected status %d", emResp.StatusCode)
	}
	emResp.Body.Close()

	waitForLegState(t, instA.baseURL(), outboundLeg.ID, "early_media", 5*time.Second)

	// Verify leg.joined_room event fired during early media.
	instA.collector.waitForMatch(t, events.LegJoinedRoom, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundLeg.ID && e.Data.GetRoomID() == rm.ID
	}, 3*time.Second)

	// B answers — leg stays in room, transitions to connected.
	answerResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/answer", instB.baseURL(), inboundLeg.ID), nil)
	if answerResp.StatusCode != http.StatusOK {
		t.Fatalf("answer: unexpected status %d", answerResp.StatusCode)
	}
	answerResp.Body.Close()

	waitForLegState(t, instA.baseURL(), outboundLeg.ID, "connected", 5*time.Second)

	// Room should still have exactly 1 participant (no double-add).
	getRoomResp := httpGet(t, fmt.Sprintf("%s/v1/rooms/%s", instA.baseURL(), rm.ID))
	var gotRoom roomView
	decodeJSON(t, getRoomResp, &gotRoom)
	if len(gotRoom.Participants) != 1 {
		t.Fatalf("expected 1 participant (no double-add), got %d", len(gotRoom.Participants))
	}

	// Cleanup.
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundLeg.ID))
}

func TestCreateLeg_RoomID_AutoCreateRoom(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")

	roomID := "auto-created-room"

	// Verify room does not exist yet.
	getResp := httpGet(t, fmt.Sprintf("%s/v1/rooms/%s", instA.baseURL(), roomID))
	if getResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected room to not exist, got status %d", getResp.StatusCode)
	}
	getResp.Body.Close()

	// Create leg with a non-existent room_id — room should be auto-created.
	createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type":    "sip",
		"uri":     fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"codecs":  []string{"PCMU"},
		"room_id": roomID,
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: unexpected status %d", createResp.StatusCode)
	}
	var outboundLeg legView
	decodeJSON(t, createResp, &outboundLeg)

	// Verify room was auto-created.
	instA.collector.waitForMatch(t, events.RoomCreated, func(e events.Event) bool {
		return e.Data.GetRoomID() == roomID
	}, 3*time.Second)

	getResp2 := httpGet(t, fmt.Sprintf("%s/v1/rooms/%s", instA.baseURL(), roomID))
	if getResp2.StatusCode != http.StatusOK {
		t.Fatalf("expected room to exist after auto-create, got status %d", getResp2.StatusCode)
	}
	getResp2.Body.Close()

	// Answer and verify leg joins the auto-created room.
	inboundLeg := waitForInboundLeg(t, instB.baseURL(), 5*time.Second)
	answerResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/answer", instB.baseURL(), inboundLeg.ID), nil)
	if answerResp.StatusCode != http.StatusOK {
		t.Fatalf("answer: unexpected status %d", answerResp.StatusCode)
	}
	answerResp.Body.Close()

	waitForLegState(t, instA.baseURL(), outboundLeg.ID, "connected", 5*time.Second)

	instA.collector.waitForMatch(t, events.LegJoinedRoom, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundLeg.ID && e.Data.GetRoomID() == roomID
	}, 3*time.Second)

	// Cleanup.
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundLeg.ID))
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

// ---------------------------------------------------------------------------
// Session Timer Tests (RFC 4028)
// ---------------------------------------------------------------------------

// TestSessionTimer_CallConnectsWithHeaders verifies that an outbound call
// carrying Session-Expires and Supported: timer headers connects normally and
// that the inbound leg's session timer fields are populated.
func TestSessionTimer_CallConnectsWithHeaders(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")

	// A dials B with session timer headers.
	createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type":   "sip",
		"uri":    fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"codecs": []string{"PCMU"},
		"headers": map[string]string{
			"Session-Expires": "1800;refresher=uac",
			"Supported":       "timer",
			"Min-SE":          "90",
		},
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: unexpected status %d", createResp.StatusCode)
	}
	var outbound legView
	decodeJSON(t, createResp, &outbound)

	// Wait for inbound on B and answer.
	inbound := waitForInboundLeg(t, instB.baseURL(), 5*time.Second)
	answerResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/answer", instB.baseURL(), inbound.ID), nil)
	if answerResp.StatusCode != http.StatusOK {
		t.Fatalf("answer: unexpected status %d", answerResp.StatusCode)
	}
	answerResp.Body.Close()

	// Both legs should reach connected state.
	waitForLegState(t, instA.baseURL(), outbound.ID, "connected", 5*time.Second)
	waitForLegState(t, instB.baseURL(), inbound.ID, "connected", 5*time.Second)

	// Verify the inbound leg on B has session timer fields set.
	var sipLeg *leg.SIPLeg
	for _, l := range instB.legMgr.List() {
		if sl, ok := l.(*leg.SIPLeg); ok && sl.ID() == inbound.ID {
			sipLeg = sl
			break
		}
	}
	if sipLeg == nil {
		t.Fatal("inbound SIP leg not found in manager")
	}
	interval, refresher := sipLeg.SessionTimerParams()
	if interval != 1800 {
		t.Fatalf("expected session interval 1800, got %d", interval)
	}
	if refresher != "uac" {
		t.Fatalf("expected refresher uac, got %q", refresher)
	}

	// Cleanup.
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outbound.ID))
}

// TestSessionTimer_RefreshReInvite verifies that a re-INVITE (e.g. hold/unhold)
// resets the session timer on the remote side.
func TestSessionTimer_RefreshReInvite(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")

	// Establish call with session timer headers from A.
	createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type":   "sip",
		"uri":    fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"codecs": []string{"PCMU"},
		"headers": map[string]string{
			"Session-Expires": "1800;refresher=uac",
			"Supported":       "timer",
		},
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

	// Hold from A → triggers re-INVITE to B, which resets B's session timer.
	holdResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/hold", instA.baseURL(), outbound.ID), nil)
	if holdResp.StatusCode != http.StatusOK {
		t.Fatalf("hold: unexpected status %d", holdResp.StatusCode)
	}
	holdResp.Body.Close()

	// B should see the hold event (re-INVITE was processed → timer was reset).
	instB.collector.waitForMatch(t, events.LegHold, func(e events.Event) bool {
		return e.Data.GetLegID() == inbound.ID
	}, 5*time.Second)

	// Unhold from A.
	unholdResp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s/hold", instA.baseURL(), outbound.ID))
	if unholdResp.StatusCode != http.StatusOK {
		t.Fatalf("unhold: unexpected status %d", unholdResp.StatusCode)
	}
	unholdResp.Body.Close()

	instB.collector.waitForMatch(t, events.LegUnhold, func(e events.Event) bool {
		return e.Data.GetLegID() == inbound.ID
	}, 5*time.Second)

	// Verify both legs are still connected (session wasn't terminated).
	waitForLegState(t, instA.baseURL(), outbound.ID, "connected", 3*time.Second)
	waitForLegState(t, instB.baseURL(), inbound.ID, "connected", 3*time.Second)

	// Cleanup.
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outbound.ID))
}

// TestSessionTimer_NoTimerWithoutHeader verifies that calls without
// Session-Expires headers don't activate session timers.
func TestSessionTimer_NoTimerWithoutHeader(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")

	outboundID, inboundID := establishCall(t, instA, instB)

	// Verify inbound leg has no session timer.
	var sipLeg *leg.SIPLeg
	for _, l := range instB.legMgr.List() {
		if sl, ok := l.(*leg.SIPLeg); ok && sl.ID() == inboundID {
			sipLeg = sl
			break
		}
	}
	if sipLeg == nil {
		t.Fatal("inbound SIP leg not found")
	}
	interval, _ := sipLeg.SessionTimerParams()
	if interval != 0 {
		t.Fatalf("expected no session timer (interval=0), got %d", interval)
	}

	// Cleanup.
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
	_ = inboundID
}
