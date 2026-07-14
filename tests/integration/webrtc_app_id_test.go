//go:build integration

package integration

import (
	"net/http"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// newWebRTCClient builds a pion client peer connection with one audio
// transceiver and returns it alongside its ICE candidate channel and a channel
// closed once the connection is established.
func newWebRTCClient(t *testing.T) (*webrtc.PeerConnection, chan webrtc.ICECandidateInit, <-chan struct{}) {
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

	candCh := make(chan webrtc.ICECandidateInit, 16)
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			close(candCh)
			return
		}
		select {
		case candCh <- c.ToJSON():
		default:
		}
	})

	connected := make(chan struct{}, 1)
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		if s == webrtc.PeerConnectionStateConnected {
			select {
			case connected <- struct{}{}:
			default:
			}
		}
	})
	return pc, candCh, connected
}

// offerWebRTCLeg performs the SDP exchange against /v1/webrtc/offer, passing
// app_id when non-empty, and returns the new leg id.
func offerWebRTCLeg(t *testing.T, inst *testInstance, pc *webrtc.PeerConnection, appID string) string {
	t.Helper()
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("create offer: %v", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("set local desc: %v", err)
	}

	body := map[string]string{"sdp": pc.LocalDescription().SDP}
	if appID != "" {
		body["app_id"] = appID
	}
	resp := httpPost(t, inst.baseURL()+"/v1/webrtc/offer", body)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("webrtc_offer: status %d", resp.StatusCode)
	}
	var result struct {
		LegID string `json:"leg_id"`
		SDP   string `json:"sdp"`
	}
	decodeJSON(t, resp, &result)
	if result.LegID == "" {
		t.Fatal("empty leg_id")
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer, SDP: result.SDP,
	}); err != nil {
		t.Fatalf("client SetRemoteDescription: %v", err)
	}
	return result.LegID
}

// TestWebRTC_AppIDFilter proves a WebRTC leg tagged via app_id on the offer
// reaches an app_id-filtered VSI subscriber, and that an untagged WebRTC leg
// on the same server is filtered out. Without app_id on the offer request the
// leg is untagged and any non-empty filter drops its events.
func TestWebRTC_AppIDFilter(t *testing.T) {
	inst := newTestInstance(t, "webrtc-app-id")

	conn := dialVSIFiltered(t, inst, "^ptt$")
	defer conn.Close()
	readWSFrame(t, conn, 5*time.Second) // consume "connected"

	taggedPC, taggedCands, taggedConnected := newWebRTCClient(t)
	defer taggedPC.Close()
	taggedLegID := offerWebRTCLeg(t, inst, taggedPC, "ptt")

	untaggedPC, untaggedCands, untaggedConnected := newWebRTCClient(t)
	defer untaggedPC.Close()
	untaggedLegID := offerWebRTCLeg(t, inst, untaggedPC, "")

	trickleICEUntilConnected(t, inst, taggedPC, taggedLegID, taggedCands, taggedConnected, 10*time.Second)
	trickleICEUntilConnected(t, inst, untaggedPC, untaggedLegID, untaggedCands, untaggedConnected, 10*time.Second)

	// Collect what the filtered subscriber actually sees. Both legs are now
	// connected, so both would have published leg.connected by this point;
	// only the tagged one may cross the filter.
	taggedSeen, untaggedSeen := false, false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !taggedSeen {
		f, ok := tryReadWSFrame(conn, time.Until(deadline))
		if !ok {
			break
		}
		switch f.LegID {
		case taggedLegID:
			taggedSeen = true
			if f.AppID != "ptt" {
				t.Errorf("event %s: app_id = %q, want %q", f.Type, f.AppID, "ptt")
			}
		case untaggedLegID:
			untaggedSeen = true
		}
	}

	if !taggedSeen {
		t.Error("filtered subscriber never received an event for the app_id=ptt WebRTC leg")
	}
	if untaggedSeen {
		t.Error("filtered subscriber received an event for the untagged WebRTC leg; filter should drop it")
	}

	httpDelete(t, inst.baseURL()+"/v1/legs/"+taggedLegID)
	httpDelete(t, inst.baseURL()+"/v1/legs/"+untaggedLegID)
}

// TestWebRTC_AppIDInLegEvents asserts app_id set on the offer is carried on the
// leg's events for an unfiltered subscriber too.
func TestWebRTC_AppIDInLegEvents(t *testing.T) {
	inst := newTestInstance(t, "webrtc-app-id-events")

	conn := dialVSI(t, inst)
	defer conn.Close()
	readWSFrame(t, conn, 5*time.Second) // consume "connected"

	pc, cands, connected := newWebRTCClient(t)
	defer pc.Close()
	legID := offerWebRTCLeg(t, inst, pc, "dispatch")
	trickleICEUntilConnected(t, inst, pc, legID, cands, connected, 10*time.Second)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f, ok := tryReadWSFrame(conn, time.Until(deadline))
		if !ok {
			break
		}
		if f.Type == "leg.connected" && f.LegID == legID {
			if f.AppID != "dispatch" {
				t.Fatalf("leg.connected app_id = %q, want %q", f.AppID, "dispatch")
			}
			httpDelete(t, inst.baseURL()+"/v1/legs/"+legID)
			return
		}
	}
	t.Fatal("never received leg.connected for the WebRTC leg")
}
