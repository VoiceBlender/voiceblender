package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/VoiceBlender/voiceblender/internal/lifecycle"
	"github.com/VoiceBlender/voiceblender/internal/room"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
)

func newDrainTestServer(t *testing.T, mutate func(*config.Config)) *Server {
	t.Helper()
	bus := events.NewBus("test")
	log := slog.Default()
	legMgr := leg.NewManager()
	roomMgr := room.NewManager(legMgr, bus, log)
	webhooks := events.NewWebhookRegistry(bus, log, "", "")
	t.Cleanup(webhooks.Stop)
	cfg := config.Config{InstanceID: "test-instance", DefaultSampleRate: 16000, DrainRetryAfterSeconds: 30}
	if mutate != nil {
		mutate(&cfg)
	}
	return NewServer(legMgr, roomMgr, nil, bus, webhooks, nil, nil, nil, nil, cfg, nil, log)
}

func decodeStatus(t *testing.T, body []byte) InstanceStatus {
	t.Helper()
	var st InstanceStatus
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatalf("decode status: %v (%s)", err, body)
	}
	return st
}

func TestHealth_CodesFollowStatus(t *testing.T) {
	s := newDrainTestServer(t, nil)
	cases := []struct {
		name       string
		apply      func()
		live       int
		ready      int
		retryAfter string
	}{
		{"starting", func() {}, 200, 503, ""},
		{"ready", s.Lifecycle.MarkReady, 200, 200, ""},
		{"draining", func() {
			s.Lifecycle.LegCountChanged(1)
			s.Lifecycle.StartDrain(lifecycle.DrainOptions{Source: lifecycle.SourceAPI})
		}, 200, 503, "30"},
		{"drained", func() { s.Lifecycle.LegCountChanged(0) }, 200, 503, "30"},
		{"stopping", func() { s.Lifecycle.BeginStop() }, 503, 503, "30"},
	}
	for _, c := range cases {
		c.apply()
		live := doRequest(s, http.MethodGet, "/health/live", "")
		ready := doRequest(s, http.MethodGet, "/health/ready", "")
		if live.Code != c.live || ready.Code != c.ready {
			t.Errorf("%s: live=%d ready=%d, want %d/%d", c.name, live.Code, ready.Code, c.live, c.ready)
		}
		if got := ready.Header().Get("Retry-After"); got != c.retryAfter {
			t.Errorf("%s: ready Retry-After = %q, want %q", c.name, got, c.retryAfter)
		}
		for _, w := range []interface{ Bytes() []byte }{live.Body, ready.Body} {
			if st := decodeStatus(t, w.Bytes()); st.Status != c.name || st.InstanceID != "test-instance" {
				t.Errorf("%s: body status=%q instance=%q", c.name, st.Status, st.InstanceID)
			}
		}
	}
}

func TestDrainRoutes(t *testing.T) {
	s := newDrainTestServer(t, nil)
	s.Lifecycle.MarkReady()

	if w := doRequest(s, http.MethodDelete, "/v1/drain", ""); w.Code != http.StatusConflict {
		t.Fatalf("DELETE when ready = %d, want 409", w.Code)
	}
	if w := doRequest(s, http.MethodPost, "/v1/drain", `{"timeout_seconds":-1}`); w.Code != http.StatusBadRequest {
		t.Fatalf("negative timeout = %d, want 400", w.Code)
	}
	if w := doRequest(s, http.MethodPost, "/v1/drain", `{"timeout_seconds":`); w.Code != http.StatusBadRequest {
		t.Fatalf("bad JSON = %d, want 400", w.Code)
	}

	s.Lifecycle.LegCountChanged(1)
	w := doRequest(s, http.MethodPost, "/v1/drain", `{"timeout_seconds":3600}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("POST = %d, want 202; %s", w.Code, w.Body)
	}
	st := decodeStatus(t, w.Body.Bytes())
	if st.Status != "draining" || st.DrainSource != "api" || st.ExitWhenDrained || st.DrainDeadline == "" || st.DrainingSince == "" || st.Legs != 1 {
		t.Fatalf("status after POST = %+v", st)
	}

	if w := doRequest(s, http.MethodPost, "/v1/drain", ""); w.Code != http.StatusAccepted {
		t.Fatalf("second POST without body = %d, want 202", w.Code)
	}

	w = doRequest(s, http.MethodDelete, "/v1/drain", "")
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE = %d, want 200", w.Code)
	}
	if st := decodeStatus(t, w.Body.Bytes()); st.Status != "ready" || st.DrainDeadline != "" {
		t.Fatalf("status after DELETE = %+v", st)
	}

	s.Lifecycle.BeginStop()
	if w := doRequest(s, http.MethodPost, "/v1/drain", ""); w.Code != http.StatusConflict {
		t.Fatalf("POST while stopping = %d, want 409", w.Code)
	}
}

func TestDrain_CreatesAllowedWhileDrainingRefusedWhenDrained(t *testing.T) {
	s := newDrainTestServer(t, nil)
	s.Lifecycle.MarkReady()
	s.Lifecycle.LegCountChanged(1)
	s.Lifecycle.StartDrain(lifecycle.DrainOptions{Source: lifecycle.SourceAPI})

	if w := doRequest(s, http.MethodPost, "/v1/rooms", `{}`); w.Code != http.StatusCreated {
		t.Fatalf("create room while draining = %d, want 201; %s", w.Code, w.Body)
	}

	s.Lifecycle.LegCountChanged(0)
	if s.Lifecycle.State() != lifecycle.Drained {
		t.Fatalf("state = %s, want drained", s.Lifecycle.State())
	}
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "/v1/rooms", `{}`},
		{http.MethodPost, "/v1/legs", `{"type":"sip","uri":"sip:bob@127.0.0.1"}`},
		{http.MethodPost, "/v1/webrtc/offer", `{"sdp":"v=0"}`},
		{http.MethodGet, "/v1/legs/websocket", ""},
	} {
		w := doRequest(s, c.method, c.path, c.body)
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s when drained = %d, want 503; %s", c.method, c.path, w.Code, w.Body)
			continue
		}
		if got := w.Header().Get("Retry-After"); got != "30" {
			t.Errorf("%s %s Retry-After = %q, want 30", c.method, c.path, got)
		}
	}
	if w := doRequest(s, http.MethodGet, "/v1/rooms", ""); w.Code != http.StatusOK {
		t.Errorf("list rooms when drained = %d, want 200", w.Code)
	}
}

func TestCreateTrunk_UnregisterOnDrainDefault(t *testing.T) {
	s := newProxyTestServer(t, "")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		s.SIPEngine.Trunks().Shutdown(ctx)
	})
	for _, c := range []struct {
		field string
		want  bool
	}{
		{"", true},
		{`,"unregister_on_drain":true`, true},
		{`,"unregister_on_drain":false`, false},
	} {
		w := doRequest(s, http.MethodPost, "/v1/sip/trunks", `{"type":"sip_register","sip_register":{
			"registrar_uri":"sip:127.0.0.1:9","aor":"sip:alice@127.0.0.1","password":"secret"`+c.field+`}}`)
		if w.Code != http.StatusAccepted {
			t.Fatalf("create %q = %d; %s", c.field, w.Code, w.Body)
		}
		var res CreateTrunkResponse
		_ = json.Unmarshal(w.Body.Bytes(), &res)
		w = doRequest(s, http.MethodGet, "/v1/sip/trunks/"+res.ID, "")
		var view sipmod.TrunkView
		if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		if view.SIPRegister == nil || view.SIPRegister.UnregisterOnDrain != c.want {
			t.Errorf("field %q: unregister_on_drain = %+v, want %v", c.field, view.SIPRegister, c.want)
		}
		doRequest(s, http.MethodDelete, "/v1/sip/trunks/"+res.ID, "")
	}
}

func TestInflightWaitsForLateWork(t *testing.T) {
	f := newInflight()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := f.wait(ctx); err != nil {
		t.Fatalf("idle wait: %v", err)
	}
	f.add()
	done := make(chan error, 1)
	go func() { done <- f.wait(ctx) }()
	time.Sleep(20 * time.Millisecond)
	f.add()
	f.done()
	select {
	case <-done:
		t.Fatal("wait returned with work still in flight")
	case <-time.After(20 * time.Millisecond):
	}
	f.done()
	if err := <-done; err != nil {
		t.Fatalf("wait: %v", err)
	}
}
