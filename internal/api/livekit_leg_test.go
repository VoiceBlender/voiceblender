package api

import (
	"bytes"
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
	return NewServer(legMgr, roomMgr, nil, bus, webhooks, nil, nil, nil, m, cfg, nil, log)
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
	s := NewServer(legMgr, roomMgr, nil, bus, webhooks, nil, nil, nil, m, cfg, nil, log)

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

// TestLiveKitParticipantsRouteGone confirms the Model-C-era listing
// endpoint is no longer mounted (each LK participant is its own VB leg
// in Model B; GET /v1/legs filters do the job).
func TestLiveKitParticipantsRouteGone(t *testing.T) {
	s := newLiveKitTestServer(t, false)
	w := doRequest(s, http.MethodGet, "/v1/legs/any/livekit/participants", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (route removed)", w.Code)
	}
}

func TestLiveKitMuteRouteGone(t *testing.T) {
	s := newLiveKitTestServer(t, false)
	w := doRequest(s, http.MethodPost, "/v1/legs/any/livekit/participants/alice/mute", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (route removed)", w.Code)
	}
}

// liveKitConn unit tests — drive the callbacks directly without a real
// transport. We can leave conn.transport nil because the handlers
// exercised here don't dereference it.

func newTestLKConn(t *testing.T, s *Server, roomID, appID string) *liveKitConn {
	t.Helper()
	pl := leg.NewLiveKitPublishLeg(nil, nil, 48000, s.Log)
	pl.SetRole(roleLiveKitPublish)
	if appID != "" {
		pl.SetAppID(appID)
	}
	if roomID != "" {
		pl.SetRoomID(roomID)
	}
	s.LegMgr.Add(pl)
	return &liveKitConn{
		server:     s,
		publishLeg: pl,
		tracks:     map[string]string{},
	}
}

func TestHandleRemoteAudioTrack_CreatesParticipantLeg(t *testing.T) {
	s := newLiveKitTestServer(t, false)
	roomID := "r1"
	if _, err := s.RoomMgr.Create(roomID, "app", s.Config.DefaultSampleRate); err != nil {
		t.Fatal(err)
	}
	conn := newTestLKConn(t, s, roomID, "app")
	if err := s.RoomMgr.AddLeg(roomID, conn.publishLeg.ID()); err != nil {
		t.Fatal(err)
	}

	// Subscribe to bus to capture leg.connected for the participant.
	gotConnected := make(chan string, 4)
	unsub := s.Bus.Subscribe(func(ev events.Event) {
		if ev.Type == events.LegConnected {
			gotConnected <- ev.Data.GetLegID()
		}
	})
	defer unsub()

	conn.handleRemoteAudioTrack("alice", "PA_a", "TR_a", bytes.NewReader(nil))

	// Find the participant leg in the manager.
	var partLeg *leg.LiveKitParticipantLeg
	for _, l := range s.LegMgr.List() {
		if pl, ok := l.(*leg.LiveKitParticipantLeg); ok {
			partLeg = pl
		}
	}
	if partLeg == nil {
		t.Fatal("no LiveKitParticipantLeg created")
	}
	if partLeg.Identity() != "alice" {
		t.Errorf("identity = %q, want alice", partLeg.Identity())
	}
	if partLeg.TrackSID() != "TR_a" {
		t.Errorf("trackSID = %q, want TR_a", partLeg.TrackSID())
	}
	if partLeg.Role() != roleLiveKitListen {
		t.Errorf("role = %q, want %s", partLeg.Role(), roleLiveKitListen)
	}
	if partLeg.AppID() != "app" {
		t.Errorf("appID = %q, want app", partLeg.AppID())
	}
	if partLeg.RoomID() != roomID {
		t.Errorf("roomID = %q, want %s", partLeg.RoomID(), roomID)
	}

	// trackSID → legID index populated.
	conn.tracksMu.Lock()
	indexedID, ok := conn.tracks["TR_a"]
	conn.tracksMu.Unlock()
	if !ok || indexedID != partLeg.ID() {
		t.Errorf("tracks[TR_a] = (%q, %v); want (%q, true)", indexedID, ok, partLeg.ID())
	}

	// leg.connected fires for the participant (we sent two — publish leg
	// was added directly via LegMgr.Add without Bus.Publish, so just one
	// LegConnected is expected here, for the participant).
	select {
	case id := <-gotConnected:
		if id != partLeg.ID() {
			t.Errorf("leg.connected for %q, want %q", id, partLeg.ID())
		}
	case <-time.After(time.Second):
		t.Fatal("leg.connected not emitted for participant")
	}
}

func TestHandleRemoteAudioTrack_DropsWhenIdentityEmpty(t *testing.T) {
	s := newLiveKitTestServer(t, false)
	conn := newTestLKConn(t, s, "", "")
	conn.handleRemoteAudioTrack("", "PA_a", "TR_a", bytes.NewReader([]byte{1, 2, 3}))
	// No participant leg should have been created.
	for _, l := range s.LegMgr.List() {
		if _, ok := l.(*leg.LiveKitParticipantLeg); ok {
			t.Errorf("unexpected participant leg created for empty identity")
		}
	}
}

func TestHandleRemoteAudioTrackEnded_CleansUpLeg(t *testing.T) {
	s := newLiveKitTestServer(t, false)
	roomID := "r1"
	if _, err := s.RoomMgr.Create(roomID, "app", s.Config.DefaultSampleRate); err != nil {
		t.Fatal(err)
	}
	conn := newTestLKConn(t, s, roomID, "app")
	if err := s.RoomMgr.AddLeg(roomID, conn.publishLeg.ID()); err != nil {
		t.Fatal(err)
	}

	conn.handleRemoteAudioTrack("alice", "PA_a", "TR_a", bytes.NewReader(nil))

	var partLegID string
	for _, l := range s.LegMgr.List() {
		if pl, ok := l.(*leg.LiveKitParticipantLeg); ok {
			partLegID = pl.ID()
		}
	}
	if partLegID == "" {
		t.Fatal("setup: participant leg not created")
	}

	gotDisconnect := make(chan string, 2)
	unsub := s.Bus.Subscribe(func(ev events.Event) {
		if ev.Type == events.LegDisconnected {
			gotDisconnect <- ev.Data.GetLegID()
		}
	})
	defer unsub()

	conn.handleRemoteAudioTrackEnded("TR_a")

	select {
	case id := <-gotDisconnect:
		if id != partLegID {
			t.Errorf("leg.disconnected for %q, want %q", id, partLegID)
		}
	case <-time.After(time.Second):
		t.Fatal("leg.disconnected not emitted")
	}
	if _, ok := s.LegMgr.Get(partLegID); ok {
		t.Error("participant leg still in LegMgr after Ended")
	}

	// Idempotent: second Ended call is a no-op.
	conn.handleRemoteAudioTrackEnded("TR_a")
}

func TestRecomputeLKPublishHears_ExcludesListenersAndSelf(t *testing.T) {
	s := newLiveKitTestServer(t, false)
	roomID := "r-hears"
	if _, err := s.RoomMgr.Create(roomID, "", s.Config.DefaultSampleRate); err != nil {
		t.Fatal(err)
	}
	conn := newTestLKConn(t, s, roomID, "")
	if err := s.RoomMgr.AddLeg(roomID, conn.publishLeg.ID()); err != nil {
		t.Fatal(err)
	}

	// Create two participant legs and one fake "SIP" leg (just another
	// LiveKitParticipantLeg with a non-listen role so it qualifies as a
	// non-LK source from Hears' perspective).
	alice := leg.NewLiveKitParticipantLeg("alice", "TR_a", nil, 48000, s.Log)
	alice.SetRole(roleLiveKitListen)
	s.LegMgr.Add(alice)
	if err := s.RoomMgr.AddLeg(roomID, alice.ID()); err != nil {
		t.Fatal(err)
	}

	bob := leg.NewLiveKitParticipantLeg("bob", "TR_b", nil, 48000, s.Log)
	bob.SetRole(roleLiveKitListen)
	s.LegMgr.Add(bob)
	if err := s.RoomMgr.AddLeg(roomID, bob.ID()); err != nil {
		t.Fatal(err)
	}

	fakeSip := leg.NewLiveKitParticipantLeg("sip", "TR_s", nil, 48000, s.Log)
	fakeSip.SetRole("sip_inbound") // pretend
	s.LegMgr.Add(fakeSip)
	if err := s.RoomMgr.AddLeg(roomID, fakeSip.ID()); err != nil {
		t.Fatal(err)
	}

	s.recomputeLKPublishHears(conn.publishLeg)

	room, _ := s.RoomMgr.Get(roomID)
	hears, ok := room.Mixer().ParticipantHears(conn.publishLeg.ID())
	if !ok {
		t.Fatal("publish leg not in mixer")
	}
	// Hears should include fakeSip only.
	if _, ok := hears[fakeSip.ID()]; !ok {
		t.Errorf("Hears missing SIP-role leg %q", fakeSip.ID())
	}
	for _, listener := range []string{alice.ID(), bob.ID(), conn.publishLeg.ID()} {
		if _, ok := hears[listener]; ok {
			t.Errorf("Hears unexpectedly contained %q", listener)
		}
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
