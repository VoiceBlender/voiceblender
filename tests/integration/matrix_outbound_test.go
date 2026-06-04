//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/pion/webrtc/v4"
	mevent "maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// TestMatrixOutbound_BadCredentials confirms that the REST handler validates
// required Matrix fields before doing any network work.
func TestMatrixOutbound_BadCredentials(t *testing.T) {
	inst := newTestInstance(t, "voiceblender-mx")
	resp := httpPost(t, inst.baseURL()+"/v1/legs", map[string]any{
		"type": "matrix",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestMatrixOutbound_InviteAndHangup drives the outbound path against the
// mock homeserver: POST /v1/legs returns 201 with a ringing leg, the
// homeserver receives m.call.invite, and DELETE /v1/legs/{id} causes a
// m.call.hangup to be sent.
func TestMatrixOutbound_InviteAndHangup(t *testing.T) {
	mx := newMockHomeserver(t)
	inst := newTestInstance(t, "voiceblender-mx")

	roomID := "!room:example.org"
	body := map[string]any{
		"type":               "matrix",
		"homeserver_url":     mx.URL(),
		"access_token":       "syt_test_token",
		"matrix_user_id":     "@bot:example.org",
		"to":                 roomID,
		"matrix_lifetime_ms": 30000,
	}
	resp := httpPost(t, inst.baseURL()+"/v1/legs", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	var view map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&view); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	resp.Body.Close()
	if view["type"] != string(leg.TypeMatrixOutbound) {
		t.Errorf("type = %v, want matrix_out", view["type"])
	}
	if view["state"] != string(leg.StateRinging) {
		t.Errorf("state = %v, want ringing", view["state"])
	}

	// Confirm the homeserver saw an m.call.invite with Opus in the SDP.
	sent := mx.WaitForSent(t, "m.call.invite", 3*time.Second)
	if sent.RoomID != roomID {
		t.Errorf("invite room = %q, want %q", sent.RoomID, roomID)
	}
	var invitePayload mevent.CallInviteEventContent
	if err := json.Unmarshal(sent.Body, &invitePayload); err != nil {
		t.Fatalf("unmarshal invite: %v", err)
	}
	if !strings.Contains(strings.ToLower(invitePayload.Offer.SDP), "opus") {
		t.Errorf("invite SDP missing opus codec")
	}
	if invitePayload.Lifetime != 30000 {
		t.Errorf("lifetime = %d, want 30000", invitePayload.Lifetime)
	}

	// Hang up.
	legID, _ := view["id"].(string)
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", inst.baseURL(), legID)).Body.Close()

	sentHangup := mx.WaitForSent(t, "m.call.hangup", 3*time.Second)
	var hangupPayload mevent.CallHangupEventContent
	if err := json.Unmarshal(sentHangup.Body, &hangupPayload); err != nil {
		t.Fatalf("unmarshal hangup: %v", err)
	}
	if hangupPayload.Reason != mevent.CallHangupUserHangup {
		t.Errorf("hangup reason = %q, want user_hangup", hangupPayload.Reason)
	}

	inst.collector.waitForMatch(t, events.LegDisconnected, nil, 3*time.Second)
}

// TestMatrixOutbound_AnswerReachesConnected uses a sibling PCMedia as the
// test peer to drive a full offer/answer exchange via the mock homeserver and
// verify the leg transitions to connected with leg.connected published.
func TestMatrixOutbound_AnswerReachesConnected(t *testing.T) {
	mx := newMockHomeserver(t)
	inst := newTestInstance(t, "voiceblender-mx")

	roomID := id.RoomID("!room:example.org")
	callerUser := id.UserID("@alice:example.org")
	body := map[string]any{
		"type":               "matrix",
		"homeserver_url":     mx.URL(),
		"access_token":       "syt_test_token",
		"matrix_user_id":     "@bot:example.org",
		"to":                 string(roomID),
		"matrix_lifetime_ms": 30000,
	}
	resp := httpPost(t, inst.baseURL()+"/v1/legs", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Wait for the invite, parse the offer SDP, build a peer PCMedia, and
	// inject an answer event into the next /sync response.
	invite := mx.WaitForSent(t, "m.call.invite", 3*time.Second)
	var inviteContent mevent.CallInviteEventContent
	if err := json.Unmarshal(invite.Body, &inviteContent); err != nil {
		t.Fatalf("unmarshal invite: %v", err)
	}

	peerMedia, err := leg.NewPCMedia(leg.PCMediaConfig{
		Codec:                codec.CodecOpus,
		EnableTelephoneEvent: true,
		Log:                  inst.apiSrv.Log,
	})
	if err != nil {
		t.Fatalf("peer NewPCMedia: %v", err)
	}
	t.Cleanup(func() { peerMedia.Close() })

	pc := peerMedia.PC()
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  inviteContent.Offer.SDP,
	}); err != nil {
		t.Fatalf("peer SetRemoteDescription: %v", err)
	}
	peerAnswer, err := pc.CreateAnswer(nil)
	if err != nil {
		t.Fatalf("peer CreateAnswer: %v", err)
	}
	gatherDone := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(peerAnswer); err != nil {
		t.Fatalf("peer SetLocalDescription: %v", err)
	}
	select {
	case <-gatherDone:
	case <-time.After(5 * time.Second):
		t.Fatal("peer ICE gather timeout")
	}
	peerSDP := pc.LocalDescription().SDP

	answerEv := &mevent.Event{
		Type:      mevent.CallAnswer,
		Sender:    callerUser,
		RoomID:    roomID,
		Timestamp: time.Now().UnixMilli(),
		Content: mevent.Content{
			Parsed: &mevent.CallAnswerEventContent{
				BaseCallEventContent: mevent.BaseCallEventContent{
					CallID:  inviteContent.CallID,
					PartyID: "peer-party",
					Version: "1",
				},
				Answer: mevent.CallData{
					Type: mevent.CallDataTypeAnswer,
					SDP:  peerSDP,
				},
			},
		},
	}
	mx.InjectEvent(answerEv)

	// Wait for leg.connected.
	inst.collector.waitForMatch(t, events.LegConnected, func(e events.Event) bool {
		d, ok := e.Data.(*events.LegConnectedData)
		return ok && d.LegType == string(leg.TypeMatrixOutbound)
	}, 8*time.Second)
}
