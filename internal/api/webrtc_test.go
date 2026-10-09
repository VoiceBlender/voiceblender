package api

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/pion/webrtc/v4"
)

// makeClientOffer creates a pion peer connection on the test (client) side
// and returns a valid SDP offer that can be fed into doWebRTCOffer. The
// caller owns the returned PC and must Close() it.
func makeClientOffer(t *testing.T) (*webrtc.PeerConnection, string) {
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

func TestDoWebRTCOffer_HappyPath(t *testing.T) {
	s := newTestServer(t)
	clientPC, sdp := makeClientOffer(t)
	defer clientPC.Close()

	res, err := s.doWebRTCOffer(WebRTCOfferRequest{SDP: sdp})
	if err != nil {
		t.Fatalf("doWebRTCOffer: %v", err)
	}
	if res.LegID == "" {
		t.Fatal("empty leg_id")
	}
	if !strings.HasPrefix(res.SDP, "v=0") {
		t.Fatalf("expected SDP answer starting with v=0, got: %q", res.SDP)
	}
	got, ok := s.LegMgr.Get(res.LegID)
	if !ok {
		t.Fatal("leg not registered with manager")
	}
	if _, ok := got.(*leg.WebRTCLeg); !ok {
		t.Fatalf("registered leg is %T, want *leg.WebRTCLeg", got)
	}
}

func TestDoWebRTCOffer_AppID(t *testing.T) {
	tests := []struct {
		name  string
		appID string
		want  string
	}{
		{name: "tagged", appID: "ptt", want: "ptt"},
		{name: "omitted", appID: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(t)
			clientPC, sdp := makeClientOffer(t)
			defer clientPC.Close()

			res, err := s.doWebRTCOffer(WebRTCOfferRequest{SDP: sdp, AppID: tt.appID})
			if err != nil {
				t.Fatalf("doWebRTCOffer: %v", err)
			}
			got, ok := s.LegMgr.Get(res.LegID)
			if !ok {
				t.Fatal("leg not registered with manager")
			}
			if got.AppID() != tt.want {
				t.Errorf("leg AppID() = %q, want %q", got.AppID(), tt.want)
			}
		})
	}
}

func TestDoWebRTCOffer_InvalidSDP(t *testing.T) {
	s := newTestServer(t)
	_, err := s.doWebRTCOffer(WebRTCOfferRequest{SDP: "not an sdp"})
	if err == nil {
		t.Fatal("expected error for invalid SDP")
	}
	ae, ok := err.(*apiError)
	if !ok {
		t.Fatalf("got %T %v, want *apiError", err, err)
	}
	if ae.Code != 400 {
		t.Fatalf("Code = %d, want 400", ae.Code)
	}
}

func TestDoWebRTCAddCandidate_NotFound(t *testing.T) {
	s := newTestServer(t)
	err := s.doWebRTCAddCandidate("does-not-exist", webrtc.ICECandidateInit{
		Candidate: "candidate:1 1 udp 1 1.1.1.1 1 typ host",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	ae, ok := err.(*apiError)
	if !ok {
		t.Fatalf("got %T %v, want *apiError", err, err)
	}
	if ae.Code != 404 {
		t.Fatalf("Code = %d, want 404", ae.Code)
	}
}

func TestDoWebRTCGetCandidates_NotFound(t *testing.T) {
	s := newTestServer(t)
	_, err := s.doWebRTCGetCandidates("does-not-exist")
	if err == nil {
		t.Fatal("expected error")
	}
	ae, ok := err.(*apiError)
	if !ok {
		t.Fatalf("got %T %v, want *apiError", err, err)
	}
	if ae.Code != 404 {
		t.Fatalf("Code = %d, want 404", ae.Code)
	}
}

// TestDoWebRTCGetCandidates_HappyPath verifies that after a successful offer
// the server begins gathering ICE candidates and the drain endpoint surfaces
// them. Host candidates are produced without external network access, so
// this should be deterministic in any environment with a usable loopback.
func TestDoWebRTCGetCandidates_HappyPath(t *testing.T) {
	s := newTestServer(t)
	clientPC, sdp := makeClientOffer(t)
	defer clientPC.Close()

	res, err := s.doWebRTCOffer(WebRTCOfferRequest{SDP: sdp})
	if err != nil {
		t.Fatalf("doWebRTCOffer: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got, err := s.doWebRTCGetCandidates(res.LegID)
		if err != nil {
			t.Fatalf("doWebRTCGetCandidates: %v", err)
		}
		if len(got.Candidates) > 0 || got.Done {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("no ICE candidates gathered within deadline")
}

func TestDoWebRTCAddCandidate_HappyPath(t *testing.T) {
	s := newTestServer(t)
	clientPC, sdp := makeClientOffer(t)
	defer clientPC.Close()
	res, err := s.doWebRTCOffer(WebRTCOfferRequest{SDP: sdp})
	if err != nil {
		t.Fatalf("doWebRTCOffer: %v", err)
	}

	gather := webrtc.GatheringCompletePromise(clientPC)
	select {
	case <-gather:
	case <-time.After(3 * time.Second):
		t.Fatal("client ICE gathering timed out")
	}
	desc := clientPC.LocalDescription()
	if desc == nil {
		t.Fatal("client local description nil")
	}
	var candStr string
	for _, line := range strings.Split(desc.SDP, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "a=candidate:") {
			candStr = strings.TrimPrefix(line, "a=")
			break
		}
	}
	if candStr == "" {
		t.Fatal("no a=candidate line in client SDP")
	}
	mid := "0"
	idx := uint16(0)
	if err := s.doWebRTCAddCandidate(res.LegID, webrtc.ICECandidateInit{
		Candidate:     candStr,
		SDPMid:        &mid,
		SDPMLineIndex: &idx,
	}); err != nil {
		t.Fatalf("doWebRTCAddCandidate: %v", err)
	}
}

// TestVSIMetadata_WebRTCRegistered ensures every WebRTC VSI command is
// registered in VSICommandsMetadata so asyncapi-gen emits them.
func TestVSIMetadata_WebRTCRegistered(t *testing.T) {
	want := map[string]bool{
		"webrtc_offer":          false,
		"webrtc_add_candidate":  false,
		"webrtc_get_candidates": false,
		"webrtc_ice_restart":    false,
	}
	for _, cmd := range VSICommandsMetadata() {
		if _, ok := want[cmd.Name]; ok {
			want[cmd.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("VSI command %q missing from VSICommandsMetadata", name)
		}
	}
}

func sdpUfrag(t *testing.T, sdp string) string {
	t.Helper()
	for _, line := range strings.Split(sdp, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "a=ice-ufrag:"); ok {
			return v
		}
	}
	t.Fatal("no a=ice-ufrag line in SDP")
	return ""
}

func wantAPIErrorCode(t *testing.T, err error, code int) {
	t.Helper()
	var apiErr *apiError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *apiError with code %d", err, code)
	}
	if apiErr.Code != code {
		t.Errorf("code = %d (%s), want %d", apiErr.Code, apiErr.Message, code)
	}
}

func TestDoWebRTCICERestart_HappyPath(t *testing.T) {
	s := newTestServer(t)
	clientPC, sdp := makeClientOffer(t)
	defer clientPC.Close()
	res, err := s.doWebRTCOffer(WebRTCOfferRequest{SDP: sdp})
	if err != nil {
		t.Fatalf("doWebRTCOffer: %v", err)
	}
	if err := clientPC.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: res.SDP}); err != nil {
		t.Fatalf("client SetRemoteDescription: %v", err)
	}

	// Drain the first gathering round so new candidates can only come from the restart.
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := s.doWebRTCGetCandidates(res.LegID)
		if err != nil {
			t.Fatalf("doWebRTCGetCandidates: %v", err)
		}
		if got.Done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("initial ICE gathering did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}

	offer, err := clientPC.CreateOffer(&webrtc.OfferOptions{ICERestart: true})
	if err != nil {
		t.Fatalf("restart CreateOffer: %v", err)
	}
	if err := clientPC.SetLocalDescription(offer); err != nil {
		t.Fatalf("restart SetLocalDescription: %v", err)
	}
	restart, err := s.doWebRTCICERestart(res.LegID, offer.SDP)
	if err != nil {
		t.Fatalf("doWebRTCICERestart: %v", err)
	}
	if restart.LegID != res.LegID {
		t.Errorf("leg_id = %q, want %q", restart.LegID, res.LegID)
	}
	if before, after := sdpUfrag(t, res.SDP), sdpUfrag(t, restart.SDP); before == after {
		t.Errorf("answer kept ice-ufrag %q; expected new ICE credentials", after)
	}
	if err := clientPC.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: restart.SDP}); err != nil {
		t.Fatalf("client SetRemoteDescription(restart answer): %v", err)
	}

	candidates := 0
	deadline = time.Now().Add(5 * time.Second)
	for {
		got, err := s.doWebRTCGetCandidates(res.LegID)
		if err != nil {
			t.Fatalf("doWebRTCGetCandidates after restart: %v", err)
		}
		candidates += len(got.Candidates)
		if got.Done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ICE gathering after restart did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if candidates == 0 {
		t.Error("no new local candidates offered after the restart")
	}
	if _, ok := s.LegMgr.Get(res.LegID); !ok {
		t.Error("leg gone after ICE restart")
	}
}

func TestDoWebRTCICERestart_Errors(t *testing.T) {
	s := newTestServer(t)
	clientPC, sdp := makeClientOffer(t)
	defer clientPC.Close()
	res, err := s.doWebRTCOffer(WebRTCOfferRequest{SDP: sdp})
	if err != nil {
		t.Fatalf("doWebRTCOffer: %v", err)
	}
	wa, _ := newWhatsAppTestLeg(t, s)

	tests := []struct {
		name  string
		legID string
		sdp   string
		code  int
	}{
		{"unknown leg", "nope", sdp, http.StatusNotFound},
		{"not a webrtc leg", wa.ID(), sdp, http.StatusBadRequest},
		{"invalid sdp", res.LegID, "not sdp", http.StatusBadRequest},
		{"empty sdp", res.LegID, "", http.StatusBadRequest},
		{"unchanged ICE credentials", res.LegID, sdp, http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := s.doWebRTCICERestart(tt.legID, tt.sdp)
			wantAPIErrorCode(t, err, tt.code)
		})
	}

	// A rejected restart must leave the leg usable.
	if _, ok := s.LegMgr.Get(res.LegID); !ok {
		t.Error("leg gone after rejected ICE restart")
	}
}

// TestEventsMetadata_ICERegistered ensures the ICE connectivity events reach
// asyncapi-gen and the webhook schema.
func TestEventsMetadata_ICERegistered(t *testing.T) {
	want := map[events.EventType]bool{
		events.LegICEInterrupted: false,
		events.LegICERestored:    false,
	}
	for _, ev := range EventsMetadata() {
		if _, ok := want[ev.Type]; ok {
			want[ev.Type] = true
		}
	}
	for typ, found := range want {
		if !found {
			t.Errorf("event %q missing from EventsMetadata", typ)
		}
	}
}
