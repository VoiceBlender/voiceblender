//go:build integration

package integration

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/api"
	"github.com/VoiceBlender/voiceblender/internal/codec"
	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/VoiceBlender/voiceblender/internal/room"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
)

// newTestInstanceIPv6 spins up a test instance bound to [::1] for SIP and HTTP.
func newTestInstanceIPv6(t *testing.T, name string) *testInstance {
	t.Helper()

	udpConn, err := net.ListenPacket("udp6", "[::1]:0")
	if err != nil {
		t.Skipf("[%s] cannot bind UDP on [::1]: %v", name, err)
	}
	sipPort := udpConn.LocalAddr().(*net.UDPAddr).Port
	udpConn.Close()

	log := slog.Default().With("instance", name)
	recDir := t.TempDir()

	cfg := config.Config{
		SIPBindIPV6:       "::1",
		SIPListenIPV6:     "::1",
		SIPPort:           fmt.Sprintf("%d", sipPort),
		SIPHost:           name,
		HTTPAddr:          "[::1]:0",
		RecordingDir:      recDir,
		DefaultSampleRate: 16000,
	}

	bus := events.NewBus("test")
	webhooks := events.NewWebhookRegistry(bus, log, "", "")
	legMgr := leg.NewManager()
	roomMgr := room.NewManager(legMgr, bus, log)

	engine, err := sipmod.NewEngine(sipmod.EngineConfig{
		BindIPV6: "::1",
		ListenIP: "::1",
		BindPort: sipPort,
		SIPHost:  name,
		Codecs:   []codec.CodecType{codec.CodecPCMU},
		Log:      log,
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
	go func() {
		if err := engine.Serve(ctx); err != nil && ctx.Err() == nil {
			log.Error("SIP engine error", "error", err)
		}
	}()

	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		cancel()
		t.Fatalf("[%s] listen HTTP6: %v", name, err)
	}
	httpAddr := ln.Addr().String()
	httpSrv := &http.Server{Handler: apiSrv}
	go func() {
		if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Error("HTTP server error", "error", err)
		}
	}()

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
		for _, l := range legMgr.List() {
			l.Hangup(context.Background())
		}
	})

	return inst
}

// newTestInstanceDualStack spins up a test instance bound to both 127.0.0.1
// (v4) and [::1] (v6) for SIP, with HTTP on v4 only.
func newTestInstanceDualStack(t *testing.T, name string) *testInstance {
	t.Helper()

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
		SIPBindIPV6:       "::1",
		SIPListenIP:       "127.0.0.1",
		SIPListenIPV6:     "::1",
		SIPPort:           fmt.Sprintf("%d", sipPort),
		SIPHost:           name,
		HTTPAddr:          "127.0.0.1:0",
		RecordingDir:      recDir,
		DefaultSampleRate: 16000,
	}

	bus := events.NewBus("test")
	webhooks := events.NewWebhookRegistry(bus, log, "", "")
	legMgr := leg.NewManager()
	roomMgr := room.NewManager(legMgr, bus, log)

	engine, err := sipmod.NewEngine(sipmod.EngineConfig{
		BindIP:     "127.0.0.1",
		BindIPV6:   "::1",
		ListenIP:   "127.0.0.1",
		ListenIPV6: "::1",
		BindPort:   sipPort,
		SIPHost:    name,
		Codecs:     []codec.CodecType{codec.CodecPCMU},
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
	go func() {
		if err := engine.Serve(ctx); err != nil && ctx.Err() == nil {
			log.Error("SIP engine error", "error", err)
		}
	}()

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
		for _, l := range legMgr.List() {
			l.Hangup(context.Background())
		}
	})

	return inst
}

// skipIfNoIPv6Loopback skips when the host has no usable [::1] socket
// (notably some CI containers). Also skips when bindv6only=1 — the dual-stack
// RTP socket on [::]+v4 source path doesn't apply here, but native v6 should.
func skipIfNoIPv6Loopback(t *testing.T) {
	t.Helper()
	c, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback not available: %v", err)
	}
	c.Close()
}

func TestCall_IPv6Loopback(t *testing.T) {
	skipIfNoIPv6Loopback(t)

	instA := newTestInstanceIPv6(t, "v6-a")
	instB := newTestInstanceIPv6(t, "v6-b")

	// A dials B over IPv6.
	createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type":   "sip",
		"uri":    fmt.Sprintf("sip:test@%s", net.JoinHostPort("::1", fmt.Sprintf("%d", instB.sipPort))),
		"codecs": []string{"PCMU"},
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: unexpected status %d", createResp.StatusCode)
	}
	var outboundLeg legView
	decodeJSON(t, createResp, &outboundLeg)

	inboundLeg := waitForInboundLeg(t, instB.baseURL(), 5*time.Second)

	answerResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/answer", instB.baseURL(), inboundLeg.ID), nil)
	if answerResp.StatusCode != http.StatusAccepted {
		t.Fatalf("answer leg: unexpected status %d", answerResp.StatusCode)
	}
	answerResp.Body.Close()

	waitForLegState(t, instA.baseURL(), outboundLeg.ID, "connected", 5*time.Second)
	waitForLegState(t, instB.baseURL(), inboundLeg.ID, "connected", 5*time.Second)

	delResp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundLeg.ID))
	if delResp.StatusCode != http.StatusAccepted {
		t.Fatalf("delete leg: unexpected status %d", delResp.StatusCode)
	}
	delResp.Body.Close()

	instA.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundLeg.ID
	}, 5*time.Second)
	instB.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == inboundLeg.ID
	}, 5*time.Second)
}

// TestCall_DualStackInterop_V4Caller exercises the family-from-offer rule:
// a v4-only caller dials a dual-stack callee on its v4 address; the callee
// must answer with IN IP4 even though it also has a v6 advertised IP.
func TestCall_DualStackInterop_V4Caller(t *testing.T) {
	skipIfNoIPv6Loopback(t)

	instA := newTestInstance(t, "ds-v4-caller")
	instB := newTestInstanceDualStack(t, "ds-v4-callee")

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
	if answerResp.StatusCode != http.StatusAccepted {
		t.Fatalf("answer leg: unexpected status %d", answerResp.StatusCode)
	}
	answerResp.Body.Close()

	waitForLegState(t, instA.baseURL(), outboundLeg.ID, "connected", 5*time.Second)
	waitForLegState(t, instB.baseURL(), inboundLeg.ID, "connected", 5*time.Second)

	// Hang up.
	delResp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundLeg.ID))
	if delResp.StatusCode != http.StatusAccepted {
		t.Fatalf("delete leg: unexpected status %d", delResp.StatusCode)
	}
	delResp.Body.Close()
}
