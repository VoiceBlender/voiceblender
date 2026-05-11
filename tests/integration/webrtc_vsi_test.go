//go:build integration

package integration

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/pion/webrtc/v4"
)

// makeClientPC creates a pion peer connection on the test (client) side and
// returns it together with a valid SDP offer suitable for webrtc_offer over
// VSI. The caller owns the PC and must Close() it.
func makeClientPC(t *testing.T) (*webrtc.PeerConnection, string) {
	t.Helper()
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("client NewPeerConnection: %v", err)
	}
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionSendrecv,
	}); err != nil {
		pc.Close()
		t.Fatalf("AddTransceiverFromKind: %v", err)
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		pc.Close()
		t.Fatalf("CreateOffer: %v", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		pc.Close()
		t.Fatalf("SetLocalDescription: %v", err)
	}
	return pc, pc.LocalDescription().SDP
}

// firstClientCandidate extracts the first a=candidate line from a finalized
// client SDP and returns its candidate string (with the leading "a=" stripped).
func firstClientCandidate(t *testing.T, sdp string) string {
	t.Helper()
	for _, line := range strings.Split(sdp, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "a=candidate:") {
			return strings.TrimPrefix(line, "a=")
		}
	}
	t.Fatal("no a=candidate line in client SDP")
	return ""
}

// TestVSI_WebRTC_FullFlow exercises the three webrtc_* commands end-to-end
// over the /v1/vsi WebSocket: offer, drain candidates, add a remote
// candidate, and verify the resulting leg appears in list_legs and emits a
// leg.connected event on the bus.
func TestVSI_WebRTC_FullFlow(t *testing.T) {
	inst := newTestInstance(t, "vsi-webrtc")

	conn := dialVSI(t, inst)
	defer conn.Close()
	readWSFrame(t, conn, 5*time.Second) // consume "connected"

	clientPC, sdp := makeClientPC(t)
	defer clientPC.Close()

	// 1. webrtc_offer
	f := vsiSend(t, conn, "webrtc_offer", "off-1", map[string]string{"sdp": sdp})
	if f.Type != "webrtc_offer.result" {
		t.Fatalf("type = %q, want webrtc_offer.result", f.Type)
	}
	var offerResult struct {
		LegID string `json:"leg_id"`
		SDP   string `json:"sdp"`
	}
	if err := json.Unmarshal(f.Data, &offerResult); err != nil {
		t.Fatalf("decode offer result: %v (%s)", err, f.Data)
	}
	if offerResult.LegID == "" || !strings.HasPrefix(offerResult.SDP, "v=0") {
		t.Fatalf("unexpected offer result: %+v", offerResult)
	}

	// Apply server's answer to the client PC so the bridge can complete
	// negotiation; this is what a real browser would do.
	if err := clientPC.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  offerResult.SDP,
	}); err != nil {
		t.Fatalf("client SetRemoteDescription: %v", err)
	}

	// Confirm the leg also exists via list_legs.
	listFrame := vsiSend(t, conn, "list_legs", "list-1", nil)
	if listFrame.Type != "list_legs.result" {
		t.Fatalf("list_legs type = %q", listFrame.Type)
	}
	var legs []legView
	if err := json.Unmarshal(listFrame.Data, &legs); err != nil {
		t.Fatalf("decode list_legs: %v", err)
	}
	found := false
	for _, l := range legs {
		if l.ID == offerResult.LegID && l.Type == "webrtc" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("webrtc leg %s not in list_legs result: %+v", offerResult.LegID, legs)
	}

	// leg.connected should have been emitted on the bus by doWebRTCOffer.
	inst.collector.waitForMatch(t, events.LegConnected, func(e events.Event) bool {
		return e.Data.GetLegID() == offerResult.LegID
	}, 2*time.Second)

	// 2. webrtc_get_candidates — poll until either candidates appear or
	// gathering reports done. Host candidates are produced without external
	// network access, so this completes promptly on loopback.
	deadline := time.Now().Add(3 * time.Second)
	gotAny := false
	for time.Now().Before(deadline) {
		gf := vsiSend(t, conn, "webrtc_get_candidates", "cand-poll", map[string]string{"id": offerResult.LegID})
		if gf.Type != "webrtc_get_candidates.result" {
			t.Fatalf("get_candidates type = %q", gf.Type)
		}
		var got struct {
			Candidates []json.RawMessage `json:"candidates"`
			Done       bool              `json:"done"`
		}
		if err := json.Unmarshal(gf.Data, &got); err != nil {
			t.Fatalf("decode get_candidates: %v", err)
		}
		if len(got.Candidates) > 0 {
			gotAny = true
		}
		if got.Done || gotAny {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !gotAny {
		t.Fatal("server produced no ICE candidates within 3s")
	}

	// 3. webrtc_add_candidate — wait for client gathering to finish, push
	// one candidate up to the server.
	gather := webrtc.GatheringCompletePromise(clientPC)
	select {
	case <-gather:
	case <-time.After(3 * time.Second):
		t.Fatal("client ICE gathering timed out")
	}
	candStr := firstClientCandidate(t, clientPC.LocalDescription().SDP)
	mid := "0"
	idx := uint16(0)
	addFrame := vsiSend(t, conn, "webrtc_add_candidate", "add-1", map[string]interface{}{
		"id": offerResult.LegID,
		"candidate": map[string]interface{}{
			"candidate":     candStr,
			"sdpMid":        mid,
			"sdpMLineIndex": idx,
		},
	})
	if addFrame.Type != "webrtc_add_candidate.result" {
		t.Fatalf("add_candidate type = %q, data=%s", addFrame.Type, addFrame.Data)
	}
	var addStatus struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(addFrame.Data, &addStatus); err != nil {
		t.Fatalf("decode add_candidate: %v", err)
	}
	if addStatus.Status != "added" {
		t.Fatalf("add_candidate status = %q, want added", addStatus.Status)
	}

	// 4. Hang up via VSI to leave the manager clean.
	delFrame := vsiSend(t, conn, "delete_leg", "del-1", map[string]string{"id": offerResult.LegID})
	if delFrame.Type != "delete_leg.result" {
		t.Fatalf("delete_leg type = %q, data=%s", delFrame.Type, delFrame.Data)
	}
}

// TestVSI_WebRTC_OfferInvalidSDP verifies the dispatcher surfaces
// doWebRTCOffer's 400 error as a VSI error frame.
func TestVSI_WebRTC_OfferInvalidSDP(t *testing.T) {
	inst := newTestInstance(t, "vsi-webrtc-bad-sdp")
	conn := dialVSI(t, inst)
	defer conn.Close()
	readWSFrame(t, conn, 5*time.Second)

	f := vsiSend(t, conn, "webrtc_offer", "bad-1", map[string]string{"sdp": "not an sdp"})
	if f.Type != "error" {
		t.Fatalf("type = %q, want error (data=%s)", f.Type, f.Data)
	}
	var ed struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(f.Data, &ed); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if ed.Code != 400 {
		t.Fatalf("code = %d, want 400 (msg=%q)", ed.Code, ed.Message)
	}
}

// TestVSI_WebRTC_AddCandidateNotFound verifies a 404 is returned when adding
// a candidate to an unknown leg.
func TestVSI_WebRTC_AddCandidateNotFound(t *testing.T) {
	inst := newTestInstance(t, "vsi-webrtc-cand-404")
	conn := dialVSI(t, inst)
	defer conn.Close()
	readWSFrame(t, conn, 5*time.Second)

	f := vsiSend(t, conn, "webrtc_add_candidate", "nf-1", map[string]interface{}{
		"id": "no-such-leg",
		"candidate": map[string]interface{}{
			"candidate":     "candidate:1 1 udp 1 1.1.1.1 1 typ host",
			"sdpMid":        "0",
			"sdpMLineIndex": uint16(0),
		},
	})
	if f.Type != "error" {
		t.Fatalf("type = %q, want error (data=%s)", f.Type, f.Data)
	}
	var ed struct {
		Code int `json:"code"`
	}
	json.Unmarshal(f.Data, &ed)
	if ed.Code != 404 {
		t.Fatalf("code = %d, want 404", ed.Code)
	}
}
