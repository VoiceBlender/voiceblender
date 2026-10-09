//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/pion/webrtc/v4"
)

// restartClient is a pion client that can run more than one ICE gathering
// round, which the single-round helpers in webrtc_app_id_test.go cannot.
type restartClient struct {
	pc    *webrtc.PeerConnection
	track *webrtc.TrackLocalStaticRTP
	legID string

	mu      sync.Mutex
	pending []webrtc.ICECandidateInit
}

func newRestartClient(t *testing.T) *restartClient {
	t.Helper()
	pc, err := loopbackWebRTCAPI().NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("client NewPeerConnection: %v", err)
	}
	t.Cleanup(func() { pc.Close() })

	track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType:  webrtc.MimeTypeOpus,
		ClockRate: 48000,
		Channels:  2,
	}, "audio", "test-tone")
	if err != nil {
		t.Fatalf("new track: %v", err)
	}
	if _, err := pc.AddTrack(track); err != nil {
		t.Fatalf("add track: %v", err)
	}

	c := &restartClient{pc: pc, track: track}
	pc.OnICECandidate(func(cand *webrtc.ICECandidate) {
		if cand == nil {
			return
		}
		c.mu.Lock()
		c.pending = append(c.pending, cand.ToJSON())
		c.mu.Unlock()
	})
	return c
}

// connect creates the leg and trickles ICE until the client is connected.
func (c *restartClient) connect(t *testing.T, inst *testInstance) {
	t.Helper()
	c.legID = offerWebRTCLeg(t, inst, c.pc, "")
	c.trickleUntilConnected(t, inst, 10*time.Second)
	inst.collector.waitForMatch(t, events.LegConnected, func(e events.Event) bool {
		return e.Data.GetLegID() == c.legID
	}, 5*time.Second)
}

// restartOffer restarts ICE on the client only. That drops the client's
// sockets without a DTLS close, so until the offer is delivered the server
// sees what it would see from a peer that lost its network.
func (c *restartClient) restartOffer(t *testing.T) string {
	t.Helper()
	c.mu.Lock()
	c.pending = nil
	c.mu.Unlock()
	offer, err := c.pc.CreateOffer(&webrtc.OfferOptions{ICERestart: true})
	if err != nil {
		t.Fatalf("restart CreateOffer: %v", err)
	}
	if err := c.pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("restart SetLocalDescription: %v", err)
	}
	return offer.SDP
}

func (c *restartClient) applyAnswer(t *testing.T, sdp string) {
	t.Helper()
	if err := c.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sdp}); err != nil {
		t.Fatalf("client SetRemoteDescription: %v", err)
	}
}

func (c *restartClient) iceUp() bool {
	s := c.pc.ICEConnectionState()
	return s == webrtc.ICEConnectionStateConnected || s == webrtc.ICEConnectionStateCompleted
}

func (c *restartClient) trickleUntilConnected(t *testing.T, inst *testInstance, timeout time.Duration) {
	t.Helper()
	candURL := fmt.Sprintf("%s/v1/legs/%s/ice-candidates", inst.baseURL(), c.legID)
	deadline := time.Now().Add(timeout)
	for !c.iceUp() {
		if time.Now().After(deadline) {
			t.Fatalf("client ICE did not connect within %v (state %s)", timeout, c.pc.ICEConnectionState())
		}
		c.mu.Lock()
		out := c.pending
		c.pending = nil
		c.mu.Unlock()
		for _, cand := range out {
			body, _ := json.Marshal(cand)
			r := httpPost(t, candURL, json.RawMessage(body))
			r.Body.Close()
		}

		var got struct {
			Candidates []webrtc.ICECandidateInit `json:"candidates"`
		}
		decodeJSON(t, httpGet(t, candURL), &got)
		for _, cand := range got.Candidates {
			if err := c.pc.AddICECandidate(cand); err != nil {
				t.Logf("client AddICECandidate: %v", err)
			}
		}
		time.Sleep(30 * time.Millisecond)
	}
}

func sdpICEUfrag(t *testing.T, sdp string) string {
	t.Helper()
	for _, line := range strings.Split(sdp, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "a=ice-ufrag:"); ok {
			return v
		}
	}
	t.Fatal("no a=ice-ufrag line in SDP")
	return ""
}

type iceRestartResult struct {
	LegID string `json:"leg_id"`
	SDP   string `json:"sdp"`
}

func legEvent(legID string) func(events.Event) bool {
	return func(e events.Event) bool { return e.Data.GetLegID() == legID }
}

// TestWebRTC_ICERestartKeepsLegAndMedia drops a connected WebRTC client's
// network path, restarts ICE over REST, and proves the same leg carries audio
// into its room again without ever being reported as disconnected.
func TestWebRTC_ICERestartKeepsLegAndMedia(t *testing.T) {
	inst := newTestInstanceWithOpts(t, "webrtc-ice-restart", func(cfg *config.Config) {
		cfg.ICEDisconnectedTimeout = time.Second
		cfg.ICEFailedTimeout = 30 * time.Second
	})
	c := newRestartClient(t)
	c.connect(t, inst)

	roomResp := httpPost(t, inst.baseURL()+"/v1/rooms", map[string]interface{}{})
	if roomResp.StatusCode != http.StatusCreated {
		t.Fatalf("create room: status %d", roomResp.StatusCode)
	}
	var rm roomView
	decodeJSON(t, roomResp, &rm)
	addResp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/legs", inst.baseURL(), rm.ID),
		map[string]string{"leg_id": c.legID})
	if addResp.StatusCode != http.StatusOK {
		t.Fatalf("add leg to room: status %d", addResp.StatusCode)
	}
	addResp.Body.Close()

	stopTone := make(chan struct{})
	toneDone := make(chan struct{})
	toneErr := make(chan error, 1)
	go pumpToneRTP(c.track, 1000.0, stopTone, toneDone, toneErr)
	defer func() {
		close(stopTone)
		<-toneDone
		select {
		case err := <-toneErr:
			t.Errorf("tone pump: %v", err)
		default:
		}
	}()

	firstAnswerUfrag := sdpICEUfrag(t, c.pc.RemoteDescription().SDP)
	offerSDP := c.restartOffer(t)
	inst.collector.waitForMatch(t, events.LegICEInterrupted, legEvent(c.legID), 10*time.Second)

	resp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/ice-restart", inst.baseURL(), c.legID),
		map[string]string{"sdp": offerSDP})
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("ice-restart: status %d", resp.StatusCode)
	}
	var restart iceRestartResult
	decodeJSON(t, resp, &restart)
	if restart.LegID != c.legID {
		t.Errorf("ice-restart leg_id = %q, want %q", restart.LegID, c.legID)
	}
	if got := sdpICEUfrag(t, restart.SDP); got == firstAnswerUfrag {
		t.Errorf("restart answer kept ice-ufrag %q; expected new ICE credentials", got)
	}
	c.applyAnswer(t, restart.SDP)
	c.trickleUntilConnected(t, inst, 10*time.Second)
	inst.collector.waitForMatch(t, events.LegICERestored, legEvent(c.legID), 5*time.Second)

	// Recording starts only after the restart, so a tone in it crossed the new path.
	time.Sleep(300 * time.Millisecond)
	recResp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/record", inst.baseURL(), rm.ID), map[string]interface{}{})
	if recResp.StatusCode != http.StatusOK {
		t.Fatalf("start recording: status %d", recResp.StatusCode)
	}
	recResp.Body.Close()
	time.Sleep(time.Second)
	stopResp := httpDelete(t, fmt.Sprintf("%s/v1/rooms/%s/record", inst.baseURL(), rm.ID))
	if stopResp.StatusCode != http.StatusOK {
		t.Fatalf("stop recording: status %d", stopResp.StatusCode)
	}
	var recStop recordingResponse
	decodeJSON(t, stopResp, &recStop)
	assertToneInWAV(t, recStop.File, 16000, 1000.0)

	if n := len(inst.collector.matchAll(events.LegConnected, legEvent(c.legID))); n != 1 {
		t.Errorf("leg.connected fired %d times, want 1", n)
	}
	if inst.collector.hasEvent(events.LegDisconnected, legEvent(c.legID)) {
		t.Error("leg.disconnected fired although ICE was restarted in time")
	}
	legResp := httpGet(t, inst.baseURL()+"/v1/legs/"+c.legID)
	var lv legView
	decodeJSON(t, legResp, &lv)
	if legResp.StatusCode != http.StatusOK || lv.RoomID != rm.ID {
		t.Errorf("leg after restart: status %d room %q, want 200 in room %q", legResp.StatusCode, lv.RoomID, rm.ID)
	}

	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", inst.baseURL(), c.legID))
}

// TestWebRTC_ICEFailureAfterInterruption proves a leg whose peer never
// restarts ICE is reported as interrupted first and torn down only once ICE
// fails.
func TestWebRTC_ICEFailureAfterInterruption(t *testing.T) {
	inst := newTestInstanceWithOpts(t, "webrtc-ice-failure", func(cfg *config.Config) {
		cfg.ICEDisconnectedTimeout = time.Second
		cfg.ICEFailedTimeout = 2 * time.Second
	})
	c := newRestartClient(t)
	c.connect(t, inst)

	c.restartOffer(t)
	interrupted := inst.collector.waitForMatch(t, events.LegICEInterrupted, legEvent(c.legID), 10*time.Second)
	if inst.collector.hasEvent(events.LegDisconnected, legEvent(c.legID)) {
		t.Fatal("leg.disconnected fired at ICE disconnected; the leg must survive until ICE fails")
	}
	if lt := interrupted.Data.(*events.LegICEInterruptedData).LegType; lt != "webrtc" {
		t.Errorf("leg.ice_interrupted leg_type = %q, want webrtc", lt)
	}

	disc := inst.collector.waitForMatch(t, events.LegDisconnected, legEvent(c.legID), 10*time.Second)
	if got := disc.Data.(*events.LegDisconnectedData).CDR.Reason; got != "ice_failure" {
		t.Errorf("cdr.reason = %q, want ice_failure", got)
	}
	if inst.collector.hasEvent(events.LegICERestored, legEvent(c.legID)) {
		t.Error("leg.ice_restored fired for a leg that never reconnected")
	}
	resp := httpGet(t, inst.baseURL()+"/v1/legs/"+c.legID)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET leg after ICE failure: status %d, want 404", resp.StatusCode)
	}
}

// TestWebRTC_ICERestartRejects covers the REST error paths of ice-restart.
func TestWebRTC_ICERestartRejects(t *testing.T) {
	inst := newTestInstance(t, "webrtc-ice-restart-rejects")
	pc, sdp := makeClientPC(t)
	defer pc.Close()
	resp := httpPost(t, inst.baseURL()+"/v1/webrtc/offer", map[string]string{"sdp": sdp})
	var offer iceRestartResult
	decodeJSON(t, resp, &offer)
	if offer.LegID == "" {
		t.Fatalf("webrtc_offer: status %d, empty leg_id", resp.StatusCode)
	}

	tests := []struct {
		name  string
		legID string
		sdp   string
		want  int
	}{
		{"unknown leg", "no-such-leg", sdp, http.StatusNotFound},
		{"invalid sdp", offer.LegID, "not an sdp", http.StatusBadRequest},
		{"unchanged ICE credentials", offer.LegID, sdp, http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/ice-restart", inst.baseURL(), tt.legID),
				map[string]string{"sdp": tt.sdp})
			r.Body.Close()
			if r.StatusCode != tt.want {
				t.Errorf("status %d, want %d", r.StatusCode, tt.want)
			}
		})
	}

	r := httpGet(t, inst.baseURL()+"/v1/legs/"+offer.LegID)
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Errorf("leg after rejected restarts: status %d, want 200", r.StatusCode)
	}
}

// TestVSI_WebRTC_ICERestart exercises webrtc_ice_restart over the /v1/vsi
// WebSocket: a restart offer returns an answer with new ICE credentials for
// the same leg, and the error paths surface as VSI error frames.
func TestVSI_WebRTC_ICERestart(t *testing.T) {
	inst := newTestInstance(t, "vsi-webrtc-ice-restart")
	conn := dialVSI(t, inst)
	defer conn.Close()
	readWSFrame(t, conn, 5*time.Second) // consume "connected"

	pc, sdp := makeClientPC(t)
	defer pc.Close()
	f := vsiSend(t, conn, "webrtc_offer", "off-1", map[string]string{"sdp": sdp})
	if f.Type != "webrtc_offer.result" {
		t.Fatalf("type = %q, want webrtc_offer.result (data=%s)", f.Type, f.Data)
	}
	var offer iceRestartResult
	if err := json.Unmarshal(f.Data, &offer); err != nil {
		t.Fatalf("decode offer result: %v", err)
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: offer.SDP}); err != nil {
		t.Fatalf("client SetRemoteDescription: %v", err)
	}

	restartOffer, err := pc.CreateOffer(&webrtc.OfferOptions{ICERestart: true})
	if err != nil {
		t.Fatalf("restart CreateOffer: %v", err)
	}
	if err := pc.SetLocalDescription(restartOffer); err != nil {
		t.Fatalf("restart SetLocalDescription: %v", err)
	}
	f = vsiSend(t, conn, "webrtc_ice_restart", "ir-1", map[string]string{"id": offer.LegID, "sdp": restartOffer.SDP})
	if f.Type != "webrtc_ice_restart.result" {
		t.Fatalf("type = %q, want webrtc_ice_restart.result (data=%s)", f.Type, f.Data)
	}
	var restart iceRestartResult
	if err := json.Unmarshal(f.Data, &restart); err != nil {
		t.Fatalf("decode restart result: %v", err)
	}
	if restart.LegID != offer.LegID {
		t.Errorf("leg_id = %q, want %q", restart.LegID, offer.LegID)
	}
	if before, after := sdpICEUfrag(t, offer.SDP), sdpICEUfrag(t, restart.SDP); before == after {
		t.Errorf("restart answer kept ice-ufrag %q; expected new ICE credentials", after)
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: restart.SDP}); err != nil {
		t.Fatalf("client SetRemoteDescription(restart answer): %v", err)
	}

	errCases := []struct {
		name    string
		payload map[string]string
		want    int
	}{
		{"unknown leg", map[string]string{"id": "no-such-leg", "sdp": restartOffer.SDP}, 404},
		{"unchanged ICE credentials", map[string]string{"id": offer.LegID, "sdp": restartOffer.SDP}, 400},
		{"invalid sdp", map[string]string{"id": offer.LegID, "sdp": "not an sdp"}, 400},
	}
	for i, tt := range errCases {
		t.Run(tt.name, func(t *testing.T) {
			f := vsiSend(t, conn, "webrtc_ice_restart", fmt.Sprintf("ir-err-%d", i), tt.payload)
			if f.Type != "error" {
				t.Fatalf("type = %q, want error (data=%s)", f.Type, f.Data)
			}
			var ed struct {
				Code int `json:"code"`
			}
			json.Unmarshal(f.Data, &ed)
			if ed.Code != tt.want {
				t.Errorf("code = %d, want %d", ed.Code, tt.want)
			}
		})
	}
}
