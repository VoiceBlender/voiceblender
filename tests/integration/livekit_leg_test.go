//go:build integration

package integration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/VoiceBlender/voiceblender/internal/lkmedia"
)

// ---------------------------------------------------------------------------
// LiveKit integration tests
//
// These tests exercise the SIP↔LiveKit gateway end-to-end against a real
// LiveKit server. They are SKIPPED unless the following env vars are set:
//
//   LIVEKIT_TEST_URL     — wss://your-livekit-server (e.g. wss://lk.example.com)
//   LIVEKIT_TEST_KEY     — LiveKit API key (for minting test tokens)
//   LIVEKIT_TEST_SECRET  — LiveKit API secret (for minting test tokens)
//
// Spinning up a local livekit-server in Docker:
//
//   docker run --rm -p 7880:7880 -e LIVEKIT_KEYS="devkey: secret" livekit/livekit-server
//   export LIVEKIT_TEST_URL=ws://localhost:7880
//   export LIVEKIT_TEST_KEY=devkey
//   export LIVEKIT_TEST_SECRET=secret
//
// The tests use lkmedia.NewTransport directly to act as a "second client"
// joining the LK room, so they validate VB's behaviour without requiring
// a separate LiveKit client SDK.
// ---------------------------------------------------------------------------

type lkTestEnv struct {
	URL       string
	APIKey    string
	APISecret string
}

func lkTestConfig(t *testing.T) *lkTestEnv {
	t.Helper()
	url := os.Getenv("LIVEKIT_TEST_URL")
	key := os.Getenv("LIVEKIT_TEST_KEY")
	secret := os.Getenv("LIVEKIT_TEST_SECRET")
	if url == "" || key == "" || secret == "" {
		t.Skip("LIVEKIT_TEST_URL/KEY/SECRET not set — skipping LiveKit integration test")
		return nil
	}
	return &lkTestEnv{URL: url, APIKey: key, APISecret: secret}
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b[:])
}

func newLKTestInstance(t *testing.T, env *lkTestEnv) *testInstance {
	t.Helper()
	return newTestInstanceWithOpts(t, "lk-"+randomSuffix(t), func(c *config.Config) {
		c.LiveKitEnabled = true
		c.LiveKitURL = env.URL
		c.LiveKitTokenSigningEnabled = true
		c.LiveKitAPIKey = env.APIKey
		c.LiveKitAPISecret = env.APISecret
		c.LiveKitOpusBitrate = 24000
		c.LiveKitDefaultTokenTTL = 30 * time.Minute
	})
}

// lkLegView is a local copy of the LegView shape extended with the
// Headers map we need to assert on.
type lkLegView struct {
	ID      string            `json:"id"`
	Type    string            `json:"type"`
	State   string            `json:"state"`
	RoomID  string            `json:"room_id,omitempty"`
	Role    string            `json:"role,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// TestLiveKit_PublishLegLifecycle validates the umbrella connect/disconnect
// path: POST /v1/legs creates a livekit_publish leg with the right headers
// and role; DELETE tears it down with a leg.disconnected event.
func TestLiveKit_PublishLegLifecycle(t *testing.T) {
	env := lkTestConfig(t)
	if env == nil {
		return
	}
	inst := newLKTestInstance(t, env)

	lkRoom := "vbtest-" + randomSuffix(t)
	vbRoom := "vbroom-" + randomSuffix(t)

	resp := httpPost(t, inst.baseURL()+"/v1/legs", map[string]interface{}{
		"type": "livekit_room",
		"livekit": map[string]interface{}{
			"room":             lkRoom,
			"identity":         "vb-bridge",
			"participant_name": "VoiceBlender",
		},
		"room_id": vbRoom,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create publish leg: status=%d", resp.StatusCode)
	}
	var publishLeg lkLegView
	decodeJSON(t, resp, &publishLeg)
	if publishLeg.Type != "livekit_publish" {
		t.Errorf("type = %q, want livekit_publish", publishLeg.Type)
	}
	if publishLeg.State != "connected" {
		t.Errorf("state = %q, want connected", publishLeg.State)
	}
	if publishLeg.RoomID != vbRoom {
		t.Errorf("room_id = %q, want %q", publishLeg.RoomID, vbRoom)
	}
	if publishLeg.Headers["livekit_identity"] != "vb-bridge" {
		t.Errorf("livekit_identity header = %q", publishLeg.Headers["livekit_identity"])
	}
	if publishLeg.Headers["livekit_room"] != lkRoom {
		t.Errorf("livekit_room header = %q", publishLeg.Headers["livekit_room"])
	}

	// leg.connected fired.
	inst.collector.waitForMatch(t, events.LegConnected, func(e events.Event) bool {
		return e.Data.GetLegID() == publishLeg.ID
	}, 10*time.Second)

	// Tear it down.
	delResp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", inst.baseURL(), publishLeg.ID))
	if delResp.StatusCode >= 300 {
		t.Fatalf("DELETE publish leg: status=%d", delResp.StatusCode)
	}
	_ = delResp.Body.Close()
	inst.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == publishLeg.ID
	}, 15*time.Second)
}

// TestLiveKit_RemoteParticipantBecomesLeg drives a real LiveKit handshake
// from two directions: VB joins via POST /v1/legs, then a "test client"
// joins the same LK room using lkmedia.NewTransport directly. VB must
// register the test client as a livekit_participant leg. When the test
// client disconnects, the participant leg must disappear.
func TestLiveKit_RemoteParticipantBecomesLeg(t *testing.T) {
	env := lkTestConfig(t)
	if env == nil {
		return
	}
	inst := newLKTestInstance(t, env)

	lkRoom := "vbtest-" + randomSuffix(t)
	vbRoom := "vbroom-" + randomSuffix(t)

	// VB joins as "vb-bridge".
	resp := httpPost(t, inst.baseURL()+"/v1/legs", map[string]interface{}{
		"type": "livekit_room",
		"livekit": map[string]interface{}{
			"room":     lkRoom,
			"identity": "vb-bridge",
		},
		"room_id": vbRoom,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create publish leg: status=%d", resp.StatusCode)
	}
	var publishLeg lkLegView
	decodeJSON(t, resp, &publishLeg)
	inst.collector.waitForMatch(t, events.LegConnected, func(e events.Event) bool {
		return e.Data.GetLegID() == publishLeg.ID
	}, 10*time.Second)

	// Test client joins as "lk-tester".
	clientToken, err := lkmedia.MintJoinToken(env.APIKey, env.APISecret, lkmedia.JoinClaims{
		Identity:     "lk-tester",
		Room:         lkRoom,
		CanPublish:   true,
		CanSubscribe: true,
		TTL:          5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("mint client token: %v", err)
	}

	clientCtx, clientCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer clientCancel()
	clientTr, err := lkmedia.NewTransport(clientCtx,
		lkmedia.Config{OpusBitrate: 24000, Log: slog.Default()},
		lkmedia.SignalConfig{URL: env.URL, Token: clientToken, Log: slog.Default()},
		lkmedia.PeerConfig{},
		lkmedia.Callbacks{},
	)
	if err != nil {
		t.Fatalf("client transport: %v", err)
	}
	t.Cleanup(func() { _ = clientTr.CloseClient() })

	// Wait for VB to register the test client as a livekit_participant leg.
	var participantLegID string
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		for _, l := range inst.legMgr.List() {
			if l.Type() != leg.TypeLiveKitParticipant {
				continue
			}
			if l.Headers()["livekit_identity"] == "lk-tester" {
				participantLegID = l.ID()
				break
			}
		}
		if participantLegID != "" {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if participantLegID == "" {
		t.Fatal("VB never registered the LK test client as a livekit_participant leg")
	}

	// leg.connected event must have fired for the participant.
	inst.collector.waitForMatch(t, events.LegConnected, func(e events.Event) bool {
		return e.Data.GetLegID() == participantLegID
	}, 5*time.Second)

	// Verify role + roomID are correct.
	l, ok := inst.legMgr.Get(participantLegID)
	if !ok {
		t.Fatalf("participant leg disappeared")
	}
	if l.Role() != "livekit_listen" {
		t.Errorf("participant role = %q, want livekit_listen", l.Role())
	}
	if l.RoomID() != vbRoom {
		t.Errorf("participant room_id = %q, want %q", l.RoomID(), vbRoom)
	}

	// Disconnect the test client; VB should clean up the participant leg.
	_ = clientTr.CloseClient()
	ev := inst.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == participantLegID
	}, 25*time.Second)
	// Reason should be the LK-participant-left signal we emit in the API layer.
	if data, ok := ev.Data.(interface{ GetReason() string }); ok {
		if r := data.GetReason(); r != "" && !strings.Contains(r, "livekit") && !strings.Contains(r, "participant_left") {
			t.Logf("unexpected disconnect reason: %q (informational)", r)
		}
	}

	// The participant leg must be gone from LegMgr.
	if _, stillThere := inst.legMgr.Get(participantLegID); stillThere {
		t.Errorf("participant leg still in LegMgr after client disconnect")
	}
}

// TestLiveKit_BadTokenReturns502 confirms the error path: an obviously
// invalid JWT (wrong signing secret) makes the LK server reject the
// handshake, and VB returns 502 with no leg registered and no events.
func TestLiveKit_BadTokenReturns502(t *testing.T) {
	env := lkTestConfig(t)
	if env == nil {
		return
	}
	inst := newLKTestInstance(t, env)

	// Mint a token signed with a wrong secret so the server rejects it.
	bogusToken, err := lkmedia.MintJoinToken("wrong-key", "wrong-secret", lkmedia.JoinClaims{
		Identity: "vb-bridge",
		Room:     "vbtest-" + randomSuffix(t),
		TTL:      time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}

	resp := httpPost(t, inst.baseURL()+"/v1/legs", map[string]interface{}{
		"type": "livekit_room",
		"livekit": map[string]interface{}{
			"token": bogusToken,
		},
	})
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}

	// No livekit_publish leg should exist.
	for _, l := range inst.legMgr.List() {
		if l.Type() == leg.TypeLiveKitPublish {
			t.Errorf("publish leg %q registered despite failed connect", l.ID())
		}
	}
}
