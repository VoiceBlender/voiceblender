package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/VoiceBlender/voiceblender/internal/lkmedia"
	"github.com/VoiceBlender/voiceblender/internal/metrics"
	"github.com/VoiceBlender/voiceblender/internal/room"
)

// newLiveKitTestServer builds a server with LiveKit enabled but signing
// disabled — the default safe configuration.
func newLiveKitTestServer(t *testing.T, withSigning bool) *Server {
	t.Helper()
	bus := events.NewBus("lk-test")
	log := slog.Default()
	legMgr := leg.NewManager()
	roomMgr := room.NewManager(legMgr, bus, log)
	webhooks := events.NewWebhookRegistry(bus, log, "", "")
	t.Cleanup(func() { webhooks.Stop() })
	m := metrics.New(bus)

	cfg := config.Config{
		InstanceID:             "lk-test",
		DefaultSampleRate:      16000,
		LiveKitEnabled:         true,
		LiveKitURL:             "wss://lk.example.com",
		LiveKitOpusBitrate:     24000,
		LiveKitDefaultTokenTTL: 30 * time.Minute,
	}
	if withSigning {
		cfg.LiveKitTokenSigningEnabled = true
		cfg.LiveKitAPIKey = "k"
		cfg.LiveKitAPISecret = "s"
	}
	return NewServer(legMgr, roomMgr, nil, bus, webhooks, nil, nil, nil, m, cfg, log)
}

func TestCreateLiveKitRoomLeg_DisabledReturns503(t *testing.T) {
	bus := events.NewBus("t")
	log := slog.Default()
	legMgr := leg.NewManager()
	roomMgr := room.NewManager(legMgr, bus, log)
	webhooks := events.NewWebhookRegistry(bus, log, "", "")
	t.Cleanup(func() { webhooks.Stop() })
	m := metrics.New(bus)

	cfg := config.Config{InstanceID: "t", DefaultSampleRate: 16000, LiveKitEnabled: false}
	s := NewServer(legMgr, roomMgr, nil, bus, webhooks, nil, nil, nil, m, cfg, log)

	w := doRequest(s, http.MethodPost, "/v1/legs",
		`{"type":"livekit_room","livekit":{"token":"jwt"}}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503; body=%s", w.Code, w.Body.String())
	}
}

func TestCreateLiveKitRoomLeg_MissingLiveKitParams(t *testing.T) {
	s := newLiveKitTestServer(t, false)
	w := doRequest(s, http.MethodPost, "/v1/legs", `{"type":"livekit_room"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "livekit") {
		t.Errorf("expected mention of livekit in error, got: %s", w.Body.String())
	}
}

func TestCreateLiveKitRoomLeg_MissingTokenWithoutSigning(t *testing.T) {
	s := newLiveKitTestServer(t, false /* signing disabled */)
	w := doRequest(s, http.MethodPost, "/v1/legs",
		`{"type":"livekit_room","livekit":{"room":"r","identity":"i"}}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "token") {
		t.Errorf("expected mention of token in error, got: %s", w.Body.String())
	}
}

func TestCreateLiveKitRoomLeg_MissingURLNoDefault(t *testing.T) {
	s := newLiveKitTestServer(t, false)
	s.Config.LiveKitURL = "" // clear default
	w := doRequest(s, http.MethodPost, "/v1/legs",
		`{"type":"livekit_room","livekit":{"token":"jwt"}}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "URL") {
		t.Errorf("expected mention of URL in error, got: %s", w.Body.String())
	}
}

func TestResolveLiveKitToken_PassThrough(t *testing.T) {
	s := newLiveKitTestServer(t, false)
	tok, err := s.resolveLiveKitToken(&LiveKitParams{Token: "presigned"})
	if err != nil {
		t.Fatalf("resolveLiveKitToken: %v", err)
	}
	if tok != "presigned" {
		t.Errorf("token = %q, want presigned", tok)
	}
}

func TestResolveLiveKitToken_MintsWhenEnabled(t *testing.T) {
	s := newLiveKitTestServer(t, true /* signing enabled */)
	tok, err := s.resolveLiveKitToken(&LiveKitParams{
		Room:     "support",
		Identity: "vb-bridge",
	})
	if err != nil {
		t.Fatalf("resolveLiveKitToken: %v", err)
	}
	if tok == "" {
		t.Fatal("expected non-empty minted token")
	}
	// Sanity: token round-trips through JWT verification with the configured secret.
	if _, err := lkmedia.MintJoinToken("k", "s", lkmedia.JoinClaims{Identity: "i", Room: "r"}); err != nil {
		t.Fatalf("sanity mint: %v", err)
	}
}

func TestResolveLiveKitToken_MintingRequiresRoomAndIdentity(t *testing.T) {
	s := newLiveKitTestServer(t, true)
	cases := []*LiveKitParams{
		{Room: ""}, // missing both
		{Identity: "i"},
		{Room: "r"},
	}
	for _, c := range cases {
		if _, err := s.resolveLiveKitToken(c); err == nil {
			t.Errorf("expected error for %+v", c)
		}
	}
}

func TestResolveLiveKitToken_MintingRejectsInvalidTTL(t *testing.T) {
	s := newLiveKitTestServer(t, true)
	_, err := s.resolveLiveKitToken(&LiveKitParams{
		Room: "r", Identity: "i", TokenTTL: "not-a-duration",
	})
	if err == nil {
		t.Error("expected TTL parse error")
	}
}

func TestListLiveKitParticipants_LegNotFound(t *testing.T) {
	s := newLiveKitTestServer(t, false)
	w := doRequest(s, http.MethodGet, "/v1/legs/nonexistent/livekit/participants", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

// TestCreateLegRequest_LiveKitFieldDecodes ensures the JSON wire format
// round-trips through CreateLegRequest.LiveKit cleanly — important to
// catch silently-broken struct tags.
func TestCreateLegRequest_LiveKitFieldDecodes(t *testing.T) {
	body := `{
		"type":"livekit_room",
		"livekit":{
			"url":"wss://x",
			"token":"jwt",
			"room":"r",
			"identity":"i",
			"participant_name":"VoiceBlender",
			"opus_bitrate":32000,
			"token_ttl":"30m",
			"permissions":{"can_publish":true,"can_subscribe":true,"room_admin":false}
		}
	}`
	var req CreateLegRequest
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&req); err != nil {
		t.Fatal(err)
	}
	if req.LiveKit == nil {
		t.Fatal("LiveKit nil")
	}
	if req.LiveKit.URL != "wss://x" || req.LiveKit.Token != "jwt" ||
		req.LiveKit.Room != "r" || req.LiveKit.Identity != "i" {
		t.Errorf("scalar fields: %+v", req.LiveKit)
	}
	if req.LiveKit.OpusBitrate != 32000 {
		t.Errorf("opus_bitrate = %d, want 32000", req.LiveKit.OpusBitrate)
	}
	if req.LiveKit.TokenTTL != "30m" {
		t.Errorf("token_ttl = %q, want 30m", req.LiveKit.TokenTTL)
	}
	if perm := req.LiveKit.Permissions; perm == nil ||
		perm.CanPublish == nil || !*perm.CanPublish ||
		perm.CanSubscribe == nil || !*perm.CanSubscribe ||
		perm.RoomAdmin == nil || *perm.RoomAdmin {
		t.Errorf("permissions decoded wrong: %+v", req.LiveKit.Permissions)
	}
}
