//go:build integration

package integration

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/api"
	"github.com/VoiceBlender/voiceblender/internal/codec"
	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/VoiceBlender/voiceblender/internal/metrics"
	"github.com/VoiceBlender/voiceblender/internal/room"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
)

// newTestInstanceWithMetrics is like newTestInstance but wires a real
// metrics.Collector into the API server so the /metrics endpoint is active.
func newTestInstanceWithMetrics(t *testing.T, name string) *testInstance {
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

	bus := events.NewBus("test")
	webhooks := events.NewWebhookRegistry(bus, log, "", "")
	legMgr := leg.NewManager()
	roomMgr := room.NewManager(legMgr, bus, log)

	// Create a real metrics collector — this subscribes to the event bus.
	metricsCollector := metrics.New(bus)

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

	apiSrv := api.NewServer(legMgr, roomMgr, engine, bus, webhooks, nil, nil, nil, metricsCollector, cfg, log)
	engine.OnInvite(apiSrv.HandleInboundCall)
	engine.OnReInvite(apiSrv.HandleReInvite)

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

// parseGaugeValue scans the Prometheus text exposition body for a line that
// starts with metricName (not a comment line) and returns the float64 value at
// the end of that line. Returns -1 if the metric is not found.
func parseGaugeValue(t *testing.T, body, metricName string) float64 {
	t.Helper()
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := scanner.Text()
		// Skip comment / help / type lines.
		if strings.HasPrefix(line, "#") {
			continue
		}
		// Match lines that start with the exact metric name followed by a space
		// or opening brace (for labelled metrics).
		if !strings.HasPrefix(line, metricName) {
			continue
		}
		rest := line[len(metricName):]
		if len(rest) == 0 || (rest[0] != ' ' && rest[0] != '{') {
			continue
		}
		// The value is the last whitespace-separated field on the line.
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		val, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			continue
		}
		return val
	}
	return -1
}

// metricsBody fetches GET /metrics and returns the response body as a string.
func metricsBody(t *testing.T, baseURL string) string {
	t.Helper()
	resp := httpGet(t, baseURL+"/metrics")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics: unexpected status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /metrics body: %v", err)
	}
	return string(raw)
}

// ---------------------------------------------------------------------------
// Test cases
// ---------------------------------------------------------------------------

// TestMetrics_Endpoint verifies that the /metrics endpoint returns 200 and
// exposes the expected metric names.
func TestMetrics_Endpoint(t *testing.T) {
	inst := newTestInstanceWithMetrics(t, "metrics-basic")

	resp := httpGet(t, inst.baseURL()+"/metrics")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	body := string(raw)

	expected := []string{
		"voiceblender_active_legs",
		"voiceblender_active_rooms",
		"go_goroutines",
	}
	for _, name := range expected {
		if !strings.Contains(body, name) {
			t.Errorf("expected metric %q in /metrics output, but it was not found", name)
		}
	}
}

// TestMetrics_ActiveLegs establishes a call between two metrics-enabled
// instances and verifies that voiceblender_active_legs increments after
// connecting and decrements after hanging up.
func TestMetrics_ActiveLegs(t *testing.T) {
	instA := newTestInstanceWithMetrics(t, "metrics-legs-a")
	instB := newTestInstanceWithMetrics(t, "metrics-legs-b")

	// Before any call both instances should report 0 active legs.
	bodyBefore := metricsBody(t, instA.baseURL())
	before := parseGaugeValue(t, bodyBefore, "voiceblender_active_legs")
	if before < 0 {
		// Gauge is present but has never been set — prometheus emits 0.
		before = 0
	}

	outID, inID := establishCall(t, instA, instB)

	// Give the metrics collector a moment to process the events.
	time.Sleep(100 * time.Millisecond)

	// After establishing the call instA should have at least 1 active leg.
	bodyDuring := metricsBody(t, instA.baseURL())
	during := parseGaugeValue(t, bodyDuring, "voiceblender_active_legs")
	if during <= 0 {
		t.Errorf("expected voiceblender_active_legs > 0 during call, got %v", during)
	}

	// Hang up via DELETE on the outbound leg.
	delResp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outID))
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("delete leg: unexpected status %d", delResp.StatusCode)
	}
	delResp.Body.Close()

	// Wait for both disconnect events.
	instA.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == outID
	}, 5*time.Second)
	instB.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == inID
	}, 5*time.Second)

	// Give the metrics collector a moment to process the disconnect events.
	time.Sleep(100 * time.Millisecond)

	// After hangup the gauge should have decremented back.
	bodyAfter := metricsBody(t, instA.baseURL())
	after := parseGaugeValue(t, bodyAfter, "voiceblender_active_legs")
	if after < 0 {
		after = 0
	}
	if after >= during {
		t.Errorf("expected voiceblender_active_legs to decrement after hangup: during=%v after=%v", during, after)
	}

	_ = before // suppress unused warning; used for context
}

// TestMetrics_CallDuration establishes a call, lets it connect, hangs it up,
// then verifies that voiceblender_call_duration_seconds_count and
// voiceblender_disconnect_reasons_total appear in the metrics output.
func TestMetrics_CallDuration(t *testing.T) {
	instA := newTestInstanceWithMetrics(t, "metrics-dur-a")
	instB := newTestInstanceWithMetrics(t, "metrics-dur-b")

	outID, inID := establishCall(t, instA, instB)

	// Let the call run briefly so a non-zero answered duration is recorded.
	time.Sleep(200 * time.Millisecond)

	// Hang up via DELETE on the outbound leg.
	delResp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outID))
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("delete leg: unexpected status %d", delResp.StatusCode)
	}
	delResp.Body.Close()

	// Wait for disconnect events on both sides before checking metrics.
	instA.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == outID
	}, 5*time.Second)
	instB.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == inID
	}, 5*time.Second)

	// Give the metrics collector time to process the events.
	time.Sleep(100 * time.Millisecond)

	body := metricsBody(t, instA.baseURL())

	// voiceblender_call_duration_seconds_count should be present and >= 1.
	if !strings.Contains(body, "voiceblender_call_duration_seconds_count") {
		t.Error("expected voiceblender_call_duration_seconds_count in metrics output")
	} else {
		count := parseGaugeValue(t, body, "voiceblender_call_duration_seconds_count")
		if count < 1 {
			t.Errorf("expected voiceblender_call_duration_seconds_count >= 1, got %v", count)
		}
	}

	// voiceblender_disconnect_reasons_total should be present.
	if !strings.Contains(body, "voiceblender_disconnect_reasons_total") {
		t.Error("expected voiceblender_disconnect_reasons_total in metrics output")
	}
}
